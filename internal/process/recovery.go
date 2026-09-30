package process

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/journal"
	"github.com/wyw14/cry-97/internal/model"
)

func (p *Plant) RecoverLine(ctx context.Context, lineID model.LineID) (uint64, error) {
	if snapshot, found, err := p.snapshots.Load(lineID); err != nil {
		return 0, err
	} else if found {
		p.mu.Lock()
		p.lines[lineID] = snapshot
		p.mu.Unlock()
	}
	replayer, err := journal.NewReplayer(p.store, p)
	if err != nil {
		return 0, err
	}
	last, err := replayer.ReplayLine(ctx, lineID)
	if err != nil {
		return last, err
	}
	// 报警为内存态：仍在封锁的设备在重启后重建检修报警，保持封锁与报警互相可见。
	p.restoreLockoutAlarms(lineID)
	return last, nil
}

// restoreLockoutAlarms raises a maintenance alarm for every tag still active
// after a journal replay, so lockouts remain visible in the alarm queue.
func (p *Plant) restoreLockoutAlarms(lineID model.LineID) {
	existing := make(map[uuid.UUID]bool)
	for _, value := range p.alarms.All() {
		existing[value.ID] = true
	}
	for _, record := range p.lockouts.ByLine(lineID) {
		if !record.Active() || (record.AlarmID != uuid.Nil && existing[record.AlarmID]) {
			continue
		}
		message := fmt.Sprintf("%s %s 已挂牌检修（责任人 %s）：%s", record.DeviceKind, record.DeviceID, record.Owner, record.Reason)
		alarmValue := model.NewAlarm(lineID, "DEVICE_LOCKED", message, model.SeverityWarning, record.TaggedAt)
		alarmValue = p.alarms.Raise(alarmValue)
		record.AlarmID = alarmValue.ID
		p.lockouts.Restore(record)
	}
}

func (p *Plant) ApplyRecovered(ctx context.Context, event model.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.lines[event.LineID]
	if !ok {
		return errors.New("recovery line is not configured")
	}
	if event.Sequence <= state.LastEventSeq {
		return nil
	}
	if state.Emergency && event.Generation < state.Generation {
		state.LastEventSeq = event.Sequence
		p.lines[event.LineID] = state
		return nil
	}
	if event.IsTerminal() {
		var record EmergencyRecord
		if err := event.Decode(&record); err != nil {
			return err
		}
		if record.LineID != event.LineID || record.Generation != event.Generation {
			return errors.New("emergency recovery payload does not match event identity")
		}
		state = state.Isolate(event.Sequence, event.OccurredAt)
		p.lines[event.LineID] = state
		p.devices.StopLine(event.LineID, state.Generation)
		state.LastEventSeq = event.Sequence
		p.lines[event.LineID] = state
		return nil
	}
	if model.LockoutEventKinds[event.Kind] {
		var record model.LockoutRecord
		if err := event.Decode(&record); err != nil {
			return err
		}
		if err := p.applyRecoveredLockout(event.Kind, record, event.OccurredAt); err != nil {
			return err
		}
		if event.Generation > state.Generation {
			state.Generation = event.Generation
		}
		state.LastEventSeq = event.Sequence
		state.UpdatedAt = event.OccurredAt
		p.lines[event.LineID] = state
		return nil
	}
	if event.Kind == model.EventDeviceCommanded {
		var command model.DeviceCommand
		if err := event.Decode(&command); err != nil {
			return err
		}
		if err := p.devices.Publish(ctx, command); err != nil {
			return err
		}
	}
	state.LastEventSeq = event.Sequence
	state.UpdatedAt = event.OccurredAt
	p.lines[event.LineID] = state
	return nil
}

// applyRecoveredLockout rebuilds maintenance tag state, hardware lock flags and
// suspended in-flight actions while replaying the journal on startup.
func (p *Plant) applyRecoveredLockout(kind model.EventKind, record model.LockoutRecord, now time.Time) error {
	ref, ok := p.deviceCatalog[record.DeviceID]
	if !ok {
		return errors.New("recovery lockout device is not registered")
	}
	if ref.lineID != "" && ref.lineID != record.LineID {
		return errors.New("recovery lockout line does not match device catalog")
	}
	p.lockouts.Restore(record)
	switch kind {
	case model.EventDeviceLocked, model.EventLockCheckConfirmed:
		if record.Active() {
			if err := p.isolateDeviceForLock(ref, record.Generation, now); err != nil {
				return err
			}
			// restore the exact tag (with alarm/check state) after hardware isolation
			p.lockouts.Restore(record)
		} else {
			if err := p.restoreDeviceAfterLock(ref, now); err != nil {
				return err
			}
		}
	case model.EventDeviceUnlocked:
		if err := p.restoreDeviceAfterLock(ref, now); err != nil {
			return err
		}
	}
	return nil
}
