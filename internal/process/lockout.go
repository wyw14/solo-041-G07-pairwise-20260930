package process

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/maintenance"
	"github.com/wyw14/cry-97/internal/model"
)

const lockoutAlarmCode = "DEVICE_LOCKOUT"

// PlaceLockout tags a blower, pump or discharge valve out for field
// maintenance. The placement safety checklist must be fully signed off;
// the device is stopped or its in-flight actions suspended, a maintenance
// alarm is raised and the lockout survives restarts through the journal.
func (p *Plant) PlaceLockout(ctx context.Context, deviceID, reason, responsible string, confirmedChecks []string) (maintenance.Lockout, error) {
	ref, ok := p.catalog.Lookup(deviceID)
	if !ok {
		return maintenance.Lockout{}, errors.New("maintenance device is not registered")
	}
	p.mu.RLock()
	state, ok := p.lines[ref.LineID]
	p.mu.RUnlock()
	if !ok {
		return maintenance.Lockout{}, errors.New("process line is not found")
	}
	now := p.now()
	record, err := p.maintenance.Place(ref, reason, responsible, confirmedChecks, now)
	if err != nil {
		return maintenance.Lockout{}, err
	}
	persisted, err := p.appendLockoutEvent(ctx, ref, state.Generation, model.EventDeviceLockedOut,
		maintenance.LockoutRecord{Lockout: record}, now)
	if err != nil {
		p.maintenance.Drop(deviceID)
		return maintenance.Lockout{}, err
	}
	for _, action := range p.isolateForLockout(ref, now) {
		p.maintenance.AttachSuspended(deviceID, action)
	}
	raised := p.alarms.Raise(model.NewAlarm(ref.LineID, lockoutAlarmCode,
		fmt.Sprintf("%s %s 已封锁检修：%s（责任人 %s）", ref.Kind, deviceID, reason, responsible),
		model.SeverityWarning, now))
	p.maintenance.BindAlarm(deviceID, raised.ID)
	record, _ = p.maintenance.Get(deviceID)
	p.touchLine(ref.LineID, persisted.Sequence, now)
	p.persistLockouts()
	return record, nil
}

// isolateForLockout executes the safety rule for the device kind: rotating
// equipment is stopped, the discharge valve is forced shut, and in-flight
// pump actions are frozen. It returns summaries of suspended actions.
func (p *Plant) isolateForLockout(ref maintenance.DeviceRef, now time.Time) []maintenance.SuspendedAction {
	actions := make([]maintenance.SuspendedAction, 0, 3)
	switch ref.Kind {
	case model.DeviceBlower:
		if _, err := p.aeration.StopBlower(ref.ID, now); err == nil {
			p.mu.RLock()
			generation := p.lines[ref.LineID].Generation
			p.mu.RUnlock()
			p.devices.StopDevice(ref.ID, generation)
		}
	case model.DevicePump:
		if _, err := p.pumps.MaintenanceStop(ref.ID, now); err == nil {
			if sequence, suspended := p.sequences.Suspend(ref.ID, now); suspended {
				actions = append(actions, maintenance.SuspendedAction{
					Kind: "sludge_sequence", Reference: sequence.ID.String(), LineID: ref.LineID, PausedAt: now.UTC(),
				})
			}
			if handover, suspended := p.handovers.Suspend(ref.ID, now); suspended {
				actions = append(actions, maintenance.SuspendedAction{
					Kind: "pump_handover", Reference: handover.ID.String(), LineID: ref.LineID, PausedAt: now.UTC(),
				})
			}
			if items := p.compensations.SuspendDevice(ref.ID); len(items) > 0 {
				p.suspendedCompsMu.Lock()
				p.suspendedComps[ref.ID] = append(p.suspendedComps[ref.ID], items...)
				p.suspendedCompsMu.Unlock()
				for _, item := range items {
					actions = append(actions, maintenance.SuspendedAction{
						Kind: "stop_compensation", Reference: item.ID.String(), LineID: ref.LineID, PausedAt: now.UTC(),
					})
				}
			}
		}
	case model.DeviceValve:
		_ = p.valves.MaintenanceClose(ref.LineID, now)
	}
	return actions
}

// ClearLockoutResult summarises the return-to-service outcome.
type ClearLockoutResult struct {
	Lockout  maintenance.Lockout           `json:"lockout"`
	Verified string                        `json:"verification"`
	Resumed  []maintenance.SuspendedAction `json:"resumed_actions"`
	FollowUp string                        `json:"follow_up,omitempty"`
}

// ClearLockout returns a device to service. The return checklist must be
// fully confirmed and the pre-operational verification is re-executed;
// suspended actions go back to their confirmation gates and the
// maintenance alarm is recovered.
func (p *Plant) ClearLockout(ctx context.Context, deviceID string, confirmedChecks []string) (ClearLockoutResult, error) {
	ref, ok := p.catalog.Lookup(deviceID)
	if !ok {
		return ClearLockoutResult{}, errors.New("maintenance device is not registered")
	}
	active, exists := p.maintenance.Get(deviceID)
	if !exists {
		return ClearLockoutResult{}, errors.New("device has no active maintenance lockout")
	}
	p.mu.RLock()
	state, ok := p.lines[ref.LineID]
	p.mu.RUnlock()
	if !ok {
		return ClearLockoutResult{}, errors.New("process line is not found")
	}
	now := p.now()
	if _, err := p.maintenance.ConfirmReturnChecks(deviceID, confirmedChecks, now); err != nil {
		return ClearLockoutResult{}, err
	}
	verification, err := p.runPreOperationalCheck(ref, now)
	if err != nil {
		return ClearLockoutResult{}, err
	}
	record, err := p.maintenance.MarkVerified(deviceID, now)
	if err != nil {
		return ClearLockoutResult{}, err
	}
	// The lockout still blocks commands while the clearance is journaled;
	// if persistence fails the device simply stays safely tagged out.
	restore := maintenance.RestoreRecord{
		LockoutID: active.ID, Device: ref, Generation: state.Generation,
		VerifiedAt: valueTime(record.VerifiedAt, now), RestoredAt: now.UTC(),
	}
	persisted, err := p.appendLockoutEvent(ctx, ref, state.Generation, model.EventDeviceRestored, restore, now)
	if err != nil {
		return ClearLockoutResult{}, err
	}
	record, err = p.maintenance.Restore(deviceID, now)
	if err != nil {
		return ClearLockoutResult{}, err
	}
	resumed := p.resumeAfterLockout(ref, now)
	if record.AlarmID != uuid.Nil {
		_, _ = p.alarms.Recover(record.AlarmID, now)
	}
	p.touchLine(ref.LineID, persisted.Sequence, now)
	p.persistLockouts()
	return ClearLockoutResult{Lockout: record, Verified: verification, Resumed: resumed, FollowUp: followUpNote(ref.Kind)}, nil
}

// persistLockouts writes the active lockouts so they keep rejecting
// commands after a process restart.
func (p *Plant) persistLockouts() {
	if p.lockoutStore == nil {
		return
	}
	_ = p.lockoutStore.Save(p.maintenance.Active())
}

// RecoverLockouts reloads lockouts that were still active when the process
// last stopped and re-applies their safety state to the device controllers.
func (p *Plant) RecoverLockouts(now time.Time) error {
	if p.lockoutStore == nil {
		return nil
	}
	records, err := p.lockoutStore.Load()
	if err != nil {
		return err
	}
	for _, record := range records {
		if !record.Active() {
			continue
		}
		if _, ok := p.catalog.Lookup(record.Device.ID); !ok {
			continue
		}
		if err := p.maintenance.PlaceConfirmed(record); err != nil {
			return err
		}
		// Re-assert the safe state and frozen actions after a fresh boot.
		p.isolateForLockout(record.Device, now)
		// Alarms live in memory; re-raise so the alarm board still shows the
		// ongoing field work, and rebind it for automatic recovery.
		raised := p.alarms.Raise(model.NewAlarm(record.Device.LineID, lockoutAlarmCode,
			fmt.Sprintf("%s %s 封锁检修中：%s（责任人 %s）", record.Device.Kind, record.Device.ID, record.Reason, record.Responsible),
			model.SeverityWarning, now))
		p.maintenance.BindAlarm(record.Device.ID, raised.ID)
	}
	if len(records) > 0 {
		p.persistLockouts()
	}
	return nil
}

// appendLockoutEvent persists a maintenance lifecycle event on the line
// journal.
func (p *Plant) appendLockoutEvent(ctx context.Context, ref maintenance.DeviceRef, generation uint64, kind model.EventKind, payload any, now time.Time) (model.Event, error) {
	event, err := model.NewEvent(ref.LineID, generation, kind, payload, now)
	if err != nil {
		return model.Event{}, err
	}
	return p.store.Append(ctx, event)
}

// runPreOperationalCheck re-runs the pre-operational verification before a
// device is allowed back into service.
func (p *Plant) runPreOperationalCheck(ref maintenance.DeviceRef, now time.Time) (string, error) {
	switch ref.Kind {
	case model.DeviceBlower:
		blower, err := p.aeration.VerifyBlowerReady(ref.ID, now)
		if err != nil {
			return "", fmt.Errorf("pre-operational verification failed: %w", err)
		}
		if blower.Running || blower.Frequency != 0 {
			return "", errors.New("pre-operational verification failed: blower is not at safe stop")
		}
		return "风机控制链路探针通过，设备处于安全停止态", nil
	case model.DevicePump:
		for _, pump := range p.pumps.Snapshot() {
			if pump.ID != ref.ID {
				continue
			}
			if pump.Running || pump.Flow != 0 {
				return "", errors.New("pre-operational verification failed: pump is not at safe stop")
			}
			return "泵处于安全停止态，复役后需重新灌泵并确认流量", nil
		}
		return "", errors.New("pre-operational verification failed: pump is not found")
	case model.DeviceValve:
		for _, valve := range p.valves.Snapshot() {
			if valve.ID != ref.ID {
				continue
			}
			if valve.Open {
				return "", errors.New("pre-operational verification failed: discharge valve is not fully closed")
			}
			return "排水阀全行程校验记录已签署，阀位处于全关安全态", nil
		}
		return "", errors.New("pre-operational verification failed: discharge valve is not found")
	default:
		return "", errors.New("unsupported maintenance device kind")
	}
}

func (p *Plant) resumeAfterLockout(ref maintenance.DeviceRef, now time.Time) []maintenance.SuspendedAction {
	actions := make([]maintenance.SuspendedAction, 0, 3)
	if ref.Kind != model.DevicePump {
		return actions
	}
	if sequence, resumed := p.sequences.Resume(ref.LineID, ref.ID, now); resumed {
		actions = append(actions, maintenance.SuspendedAction{
			Kind: "sludge_sequence", Reference: sequence.ID.String(), LineID: ref.LineID, PausedAt: now.UTC(),
		})
	}
	if handover, resumed := p.handovers.Resume(ref.LineID, ref.ID, now); resumed {
		actions = append(actions, maintenance.SuspendedAction{
			Kind: "pump_handover", Reference: handover.ID.String(), LineID: ref.LineID, PausedAt: now.UTC(),
		})
	}
	p.suspendedCompsMu.Lock()
	items := p.suspendedComps[ref.ID]
	delete(p.suspendedComps, ref.ID)
	p.suspendedCompsMu.Unlock()
	if len(items) > 0 {
		p.compensations.Reschedule(items)
		for _, item := range items {
			actions = append(actions, maintenance.SuspendedAction{
				Kind: "stop_compensation", Reference: item.ID.String(), LineID: ref.LineID, PausedAt: now.UTC(),
			})
		}
	}
	return actions
}

func followUpNote(kind model.DeviceKind) string {
	switch kind {
	case model.DeviceBlower:
		return "风机已复役，需重新提交曝气稳定窗口后方可自动调频"
	case model.DevicePump:
		return "泵已复役，挂起的回流序列/交接退回确认门，需重新确认流量"
	case model.DeviceValve:
		return "排水阀已复役，原排放许可已失效，需凭新的化验合格结果重新签发许可"
	default:
		return ""
	}
}

// ActiveLockouts returns every lockout that still blocks commands.
func (p *Plant) ActiveLockouts() []maintenance.Lockout {
	return p.maintenance.Active()
}

// DataDir reports the plant's data directory (used for restart tests).
func (p *Plant) DataDir() string {
	return p.dataDir
}

// DeviceMaintenanceView exposes one device with its checklist and any
// active lockout.
type DeviceMaintenanceView struct {
	Device  maintenance.DeviceRef     `json:"device"`
	Lockout *maintenance.Lockout      `json:"lockout,omitempty"`
	Checks  []maintenance.SafetyCheck `json:"checklist"`
}

// MaintenanceDevices joins the catalog with the active lockout records so
// lockout state is visible alongside alarm and process-line state.
func (p *Plant) MaintenanceDevices() []DeviceMaintenanceView {
	refs := p.catalog.All()
	views := make([]DeviceMaintenanceView, 0, len(refs))
	for _, ref := range refs {
		view := DeviceMaintenanceView{Device: ref, Checks: maintenance.StandardChecks(ref.Kind)}
		if record, ok := p.maintenance.Get(ref.ID); ok {
			view.Lockout = &record
		}
		views = append(views, view)
	}
	return views
}

func (p *Plant) touchLine(lineID model.LineID, sequence uint64, now time.Time) {
	p.mu.Lock()
	state := p.lines[lineID]
	state.LastEventSeq = sequence
	state.UpdatedAt = now.UTC()
	p.lines[lineID] = state
	p.mu.Unlock()
	_ = p.snapshots.Save(state)
}

func valueTime(value *time.Time, fallback time.Time) time.Time {
	if value == nil {
		return fallback.UTC()
	}
	return value.UTC()
}
