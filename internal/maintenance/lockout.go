// Package maintenance holds the field-maintenance lockout / tag-out registry
// shared by the device controllers and the process plant.
package maintenance

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

// LockoutBook keeps the current maintenance tag of every lockable device.
//
// Safety enforcement stays disarmed until the journal has been replayed on
// startup (ArmSafety), so historical lockout events never reject recovered
// commands during recovery.
type LockoutBook struct {
	mu      sync.RWMutex
	records map[string]model.LockoutRecord
	armed   bool
}

func NewLockoutBook() *LockoutBook {
	return &LockoutBook{records: make(map[string]model.LockoutRecord)}
}

// ArmSafety turns on command blocking after recovery has finished.
func (b *LockoutBook) ArmSafety() {
	b.mu.Lock()
	b.armed = true
	b.mu.Unlock()
}

func (b *LockoutBook) safetyArmed() bool {
	return b.armed
}

// Open registers a brand new tag raised by an operator.
func (b *LockoutBook) Open(record model.LockoutRecord) error {
	if record.DeviceID == "" || record.Generation == 0 {
		return errors.New("lockout record is incomplete")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if current, exists := b.records[record.DeviceID]; exists && current.Active() {
		return model.ErrLockoutAlreadyActive
	}
	b.records[record.DeviceID] = record
	return nil
}

// Restore overwrites the tag from a replayed journal event regardless of arming.
func (b *LockoutBook) Restore(record model.LockoutRecord) {
	b.mu.Lock()
	b.records[record.DeviceID] = record
	b.mu.Unlock()
}

// ConfirmCheck signs off one safety item on the active tag.
func (b *LockoutBook) ConfirmCheck(deviceID, code, confirmedBy string, now time.Time) (model.LockoutRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.records[deviceID]
	if !ok || !record.Active() {
		return model.LockoutRecord{}, model.ErrLockoutNotFound
	}
	if err := record.ConfirmCheck(code, confirmedBy, now); err != nil {
		return model.LockoutRecord{}, err
	}
	b.records[deviceID] = record
	return record, nil
}

// MarkPreRun records a successful pre-commissioning verification.
func (b *LockoutBook) MarkPreRun(deviceID, note string, now time.Time) (model.LockoutRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.records[deviceID]
	if !ok || !record.Active() {
		return model.LockoutRecord{}, model.ErrLockoutNotFound
	}
	if err := record.MarkPreRunVerified(note, now); err != nil {
		return model.LockoutRecord{}, err
	}
	b.records[deviceID] = record
	return record, nil
}

// Release closes the active tag after checks and pre-run verification pass.
func (b *LockoutBook) Release(deviceID, releasedBy string, now time.Time) (model.LockoutRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.records[deviceID]
	if !ok || !record.Active() {
		return model.LockoutRecord{}, model.ErrLockoutNotFound
	}
	if err := record.Release(releasedBy, now); err != nil {
		return model.LockoutRecord{}, err
	}
	b.records[deviceID] = record
	return record, nil
}

// IsLocked reports whether a device currently carries an active tag.
// Before the safety gate is armed (startup replay) it always reports clear.
func (b *LockoutBook) IsLocked(deviceID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.armed {
		return false
	}
	record, ok := b.records[deviceID]
	return ok && record.Active()
}

// Blocked returns the subset of device IDs that are currently locked out.
func (b *LockoutBook) Blocked(deviceIDs []string) map[string]model.LockoutRecord {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make(map[string]model.LockoutRecord)
	if !b.armed {
		return result
	}
	for _, id := range deviceIDs {
		if record, ok := b.records[id]; ok && record.Active() {
			result[id] = record
		}
	}
	return result
}

// Get returns the latest tag (active or released) for a device.
func (b *LockoutBook) Get(deviceID string) (model.LockoutRecord, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	record, ok := b.records[deviceID]
	return record, ok
}

// ByLine returns every tag that belongs to a process line.
func (b *LockoutBook) ByLine(lineID model.LineID) []model.LockoutRecord {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]model.LockoutRecord, 0)
	for _, record := range b.records {
		if record.LineID == lineID {
			result = append(result, record)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TaggedAt.Before(result[j].TaggedAt) })
	return result
}

// All returns every tag, active tags first and most recently tagged first.
func (b *LockoutBook) All() []model.LockoutRecord {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]model.LockoutRecord, 0, len(b.records))
	for _, record := range b.records {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Active() != result[j].Active() {
			return result[i].Active()
		}
		return result[i].TaggedAt.After(result[j].TaggedAt)
	})
	return result
}
