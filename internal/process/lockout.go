package process

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// deviceRef ties a lockable device id to its kind and process line.
type deviceRef struct {
	deviceID string
	kind     model.DeviceKind
	lineID   model.LineID
}

// TagDeviceInput carries the field-maintenance tag registration.
type TagDeviceInput struct {
	DeviceID    string
	Reason      string
	Owner       string
	TaggedBy    string
	ExtraChecks []model.SafetyCheck
}

// DeviceInfo describes a lockable device and its current maintenance state.
type DeviceInfo struct {
	DeviceID string               `json:"device_id"`
	Kind     model.DeviceKind     `json:"kind"`
	LineID   model.LineID         `json:"line_id"`
	Locked   bool                 `json:"locked"`
	Record   *model.LockoutRecord `json:"record,omitempty"`
}

// registerDeviceCatalog indexes the lockable blowers, pumps and discharge valves.
func (p *Plant) registerDeviceCatalog() {
	catalog := make(map[string]deviceRef)
	for _, blower := range p.aeration.Blowers() {
		// blower id "blower-basin-<line>" serves the same line as its basin.
		lineID := model.LineID("")
		if _, ok := p.lines[lineFromBlower(blower.BasinID)]; ok {
			lineID = lineFromBlower(blower.BasinID)
		}
		catalog[blower.ID] = deviceRef{deviceID: blower.ID, kind: model.DeviceBlower, lineID: lineID}
	}
	for _, pump := range p.pumps.Snapshot() {
		catalog[pump.ID] = deviceRef{deviceID: pump.ID, kind: model.DevicePump, lineID: pump.LineID}
	}
	for _, valve := range p.valves.Snapshot() {
		catalog[valve.ID] = deviceRef{deviceID: valve.ID, kind: model.DeviceValve, lineID: valve.LineID}
	}
	p.deviceCatalog = catalog
}

func lineFromBlower(basinID string) model.LineID {
	// basin ids are shaped "basin-<line id>".
	const prefix = "basin-"
	if len(basinID) > len(prefix) && basinID[:len(prefix)] == prefix {
		return model.LineID(basinID[len(prefix):])
	}
	return model.LineID(basinID)
}

// Devices lists every lockable device with its maintenance tag state.
func (p *Plant) Devices() []DeviceInfo {
	ids := make([]string, 0, len(p.deviceCatalog))
	for id := range p.deviceCatalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]DeviceInfo, 0, len(ids))
	for _, id := range ids {
		ref := p.deviceCatalog[id]
		info := DeviceInfo{DeviceID: id, Kind: ref.kind, LineID: ref.lineID, Locked: p.lockouts.IsLocked(id)}
		if record, ok := p.lockouts.Get(id); ok {
			record := record
			info.Record = &record
		}
		result = append(result, info)
	}
	return result
}

// Lockouts returns every maintenance tag (active and released).
func (p *Plant) Lockouts() []model.LockoutRecord {
	return p.lockouts.All()
}

// LineLockouts returns the maintenance tags attached to a process line.
func (p *Plant) LineLockouts(lineID model.LineID) []model.LockoutRecord {
	return p.lockouts.ByLine(lineID)
}

// TagDevice places a field-maintenance lock on a blower, pump or discharge valve.
//
// The tag records the decommission reason, responsible owner and the mandatory
// safety checklist, drives in-flight actions to a safe state (blower/pump stop,
// valve closes and its discharge permit is suspended, sludge actions park) and
// raises a visible maintenance alarm.
func (p *Plant) TagDevice(ctx context.Context, input TagDeviceInput) (model.LockoutRecord, error) {
	ref, ok := p.deviceCatalog[input.DeviceID]
	if !ok {
		return model.LockoutRecord{}, errors.New("lockable device is not found")
	}
	if ref.lineID == "" {
		return model.LockoutRecord{}, errors.New("device process line is not configured")
	}
	if existing, exists := p.lockouts.Get(input.DeviceID); exists && existing.Active() {
		return model.LockoutRecord{}, model.ErrLockoutAlreadyActive
	}
	p.mu.RLock()
	state, lineKnown := p.lines[ref.lineID]
	p.mu.RUnlock()
	if !lineKnown {
		return model.LockoutRecord{}, errors.New("process line is not found")
	}
	now := p.now()
	generation := state.Generation + 1
	record, err := model.NewLockoutRecord(
		input.DeviceID, ref.kind, ref.lineID, input.Reason, input.Owner, input.TaggedBy,
		input.ExtraChecks, generation, now,
	)
	if err != nil {
		return model.LockoutRecord{}, err
	}
	// 先登记封锁，再按安全规则处置在途动作，最后追加事件（事件落库失败则调用方可见错误）。
	if err := p.lockouts.Open(record); err != nil {
		return model.LockoutRecord{}, err
	}
	if err := p.isolateDeviceForLock(ref, generation, now); err != nil {
		return model.LockoutRecord{}, err
	}
	message := fmt.Sprintf("%s %s 已挂牌检修（责任人 %s）：%s", ref.kind, input.DeviceID, input.Owner, input.Reason)
	alarmValue := model.NewAlarm(ref.lineID, "DEVICE_LOCKED", message, model.SeverityWarning, now)
	alarmValue = p.alarms.Raise(alarmValue)
	record.AlarmID = alarmValue.ID
	p.lockouts.Restore(record)
	event, err := model.NewEvent(ref.lineID, generation, model.EventDeviceLocked, record, now)
	if err != nil {
		return model.LockoutRecord{}, err
	}
	persisted, err := p.store.Append(ctx, event)
	if err != nil {
		return model.LockoutRecord{}, err
	}
	p.bumpLineGeneration(ref.lineID, generation, persisted.OccurredAt)
	return record, nil
}

// isolateDeviceForLock applies the kind-specific safety actions when a tag is raised.
func (p *Plant) isolateDeviceForLock(ref deviceRef, generation uint64, now time.Time) error {
	switch ref.kind {
	case model.DeviceBlower:
		if _, err := p.aeration.IsolateBlower(ref.deviceID, now); err != nil {
			return err
		}
		if _, err := p.devices.StopDevice(ref.lineID, ref.deviceID, model.DeviceBlower, generation, now); err != nil {
			return err
		}
	case model.DevicePump:
		if _, err := p.pumps.Isolate(ref.deviceID, now); err != nil {
			return err
		}
		if _, err := p.devices.StopDevice(ref.lineID, ref.deviceID, model.DevicePump, generation, now); err != nil {
			return err
		}
		p.suspendPumpActions(ref, now)
	case model.DeviceValve:
		if _, err := p.valves.Isolate(ref.lineID, now); err != nil {
			return err
		}
		if _, _, err := p.permits.SuspendLine(ref.lineID, now); err != nil {
			return err
		}
		if _, err := p.devices.StopDevice(ref.lineID, ref.deviceID, model.DeviceValve, generation, now); err != nil {
			return err
		}
	default:
		return fmt.Errorf("device kind %s cannot be locked out", ref.kind)
	}
	return nil
}

// suspendPumpAction parks the sludge sequence/handover that uses a locked pump.
func (p *Plant) suspendPumpActions(ref deviceRef, now time.Time) {
	if sequence, exists := p.sequences.PumpSequence(ref.deviceID); exists {
		_, _ = p.sequences.Suspend(sequence.LineID, now)
	}
	if p.handovers.InvolvesPump(ref.lineID, ref.deviceID) {
		_, _ = p.handovers.Suspend(ref.lineID, now)
	}
}

// ConfirmLockCheck signs off one mandatory safety item on an active tag.
func (p *Plant) ConfirmLockCheck(ctx context.Context, deviceID, code, confirmedBy string) (model.LockoutRecord, error) {
	now := p.now()
	record, err := p.lockouts.ConfirmCheck(deviceID, code, confirmedBy, now)
	if err != nil {
		return model.LockoutRecord{}, err
	}
	if err := p.appendLockoutEvent(ctx, model.EventLockCheckConfirmed, record, now); err != nil {
		return model.LockoutRecord{}, err
	}
	return record, nil
}

// VerifyPreRun re-executes the pre-commissioning checks required before release:
// every safety item signed off, line not emergency-isolated and the device
// proven stopped/closed with no in-flight command driving it.
func (p *Plant) VerifyPreRun(ctx context.Context, deviceID, note string) (model.LockoutRecord, error) {
	ref, ok := p.deviceCatalog[deviceID]
	if !ok {
		return model.LockoutRecord{}, errors.New("lockable device is not found")
	}
	record, exists := p.lockouts.Get(deviceID)
	if !exists || !record.Active() {
		return model.LockoutRecord{}, model.ErrLockoutNotFound
	}
	p.mu.RLock()
	state, lineKnown := p.lines[ref.lineID]
	p.mu.RUnlock()
	if !lineKnown {
		return model.LockoutRecord{}, errors.New("process line is not found")
	}
	if state.Emergency {
		return model.LockoutRecord{}, errors.New("pre-run verification blocked while line is emergency-isolated")
	}
	if err := p.verifyDeviceSafe(ref); err != nil {
		return model.LockoutRecord{}, err
	}
	now := p.now()
	record, err := p.lockouts.MarkPreRun(deviceID, note, now)
	if err != nil {
		return model.LockoutRecord{}, err
	}
	if err := p.appendLockoutEvent(ctx, model.EventLockCheckConfirmed, record, now); err != nil {
		return model.LockoutRecord{}, err
	}
	return record, nil
}

// verifyDeviceSafe proves the hardware is still in the safe maintenance state.
func (p *Plant) verifyDeviceSafe(ref deviceRef) error {
	switch ref.kind {
	case model.DeviceBlower:
		blocker := p.aeration.PreRunBlocker(ref.deviceID)
		if blocker != "" {
			return errors.New(blocker)
		}
	case model.DevicePump:
		blocker := p.pumpPreRunBlocker(ref.deviceID)
		if blocker != "" {
			return errors.New(blocker)
		}
	case model.DeviceValve:
		blocker := p.valvePreRunBlocker(ref.lineID)
		if blocker != "" {
			return errors.New(blocker)
		}
	}
	return nil
}

func (p *Plant) pumpPreRunBlocker(pumpID string) string {
	for _, pump := range p.pumps.Snapshot() {
		if pump.ID != pumpID {
			continue
		}
		if pump.Running {
			return "pump is still running before pre-run verification"
		}
		if !pump.Locked {
			return "pump lockout state was lost"
		}
		return ""
	}
	return "return pump is not found"
}

func (p *Plant) valvePreRunBlocker(lineID model.LineID) string {
	for _, valve := range p.valves.Snapshot() {
		if valve.LineID != lineID {
			continue
		}
		if valve.Open {
			return "discharge valve is still open before pre-run verification"
		}
		if !valve.Locked {
			return "discharge valve lockout state was lost"
		}
		return ""
	}
	return "discharge valve is not found"
}

// ReleaseDevice returns a device to service. All safety items must be signed
// off and a fresh pre-commissioning verification must have passed.
func (p *Plant) ReleaseDevice(ctx context.Context, deviceID, releasedBy string) (model.LockoutRecord, error) {
	ref, ok := p.deviceCatalog[deviceID]
	if !ok {
		return model.LockoutRecord{}, errors.New("lockable device is not found")
	}
	active, exists := p.lockouts.Get(deviceID)
	if !exists || !active.Active() {
		return model.LockoutRecord{}, model.ErrLockoutNotFound
	}
	p.mu.RLock()
	state, lineKnown := p.lines[ref.lineID]
	p.mu.RUnlock()
	if !lineKnown {
		return model.LockoutRecord{}, errors.New("process line is not found")
	}
	now := p.now()
	generation := state.Generation + 1
	record, err := p.lockouts.Release(deviceID, releasedBy, now)
	if err != nil {
		return model.LockoutRecord{}, err
	}
	if err := p.restoreDeviceAfterLock(ref, now); err != nil {
		return model.LockoutRecord{}, err
	}
	if record.AlarmID != uuid.Nil {
		if recovered, recoverErr := p.alarms.Recover(record.AlarmID, now); recoverErr == nil {
			record.AlarmID = recovered.ID
		}
	}
	p.lockouts.Restore(record)
	if err := p.appendLockoutEvent(ctx, model.EventDeviceUnlocked, record, now); err != nil {
		return model.LockoutRecord{}, err
	}
	p.bumpLineGeneration(ref.lineID, generation, now)
	return record, nil
}

// restoreDeviceAfterLock clears hardware lock flags and resumes parked actions.
func (p *Plant) restoreDeviceAfterLock(ref deviceRef, now time.Time) error {
	switch ref.kind {
	case model.DeviceBlower:
		if _, err := p.aeration.RestoreBlower(ref.deviceID, now); err != nil {
			return err
		}
	case model.DevicePump:
		if _, err := p.pumps.RestoreService(ref.deviceID, now); err != nil {
			return err
		}
		// 复役后在途动作回到“切换中”，必须由现场重新确认流量（运行前校验）。
		if sequence, exists := p.sequences.PumpSequence(ref.deviceID); exists {
			_, _ = p.sequences.Resume(sequence.LineID, now)
		}
		if p.handovers.InvolvesPump(ref.lineID, ref.deviceID) && !p.handoverPumpsLocked(ref.lineID) {
			_, _ = p.handovers.Resume(ref.lineID, now)
		}
	case model.DeviceValve:
		if _, err := p.valves.RestoreService(ref.lineID, now); err != nil {
			return err
		}
		// 排放许可已在挂牌时撤销，复役后需凭新的合格化验结果重新申领，不自动恢复。
	}
	return nil
}

func (p *Plant) handoverPumpsLocked(lineID model.LineID) bool {
	handover, ok := p.handovers.Current(lineID)
	if !ok {
		return false
	}
	return p.lockouts.IsLocked(handover.FromPump) || p.lockouts.IsLocked(handover.ToPump)
}

func (p *Plant) appendLockoutEvent(ctx context.Context, kind model.EventKind, record model.LockoutRecord, now time.Time) error {
	event, err := model.NewEvent(record.LineID, record.Generation, kind, record, now)
	if err != nil {
		return err
	}
	_, err = p.store.Append(ctx, event)
	return err
}

func (p *Plant) bumpLineGeneration(lineID model.LineID, generation uint64, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.lines[lineID]
	if !ok || generation <= state.Generation {
		return
	}
	state.Generation = generation
	state.UpdatedAt = now.UTC()
	p.lines[lineID] = state
}

// deviceLocked reports whether a device currently carries an active tag.
func (p *Plant) deviceLocked(deviceID string) bool {
	return p.lockouts.IsLocked(deviceID)
}
