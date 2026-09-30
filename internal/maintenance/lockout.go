package maintenance

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// LockoutStatus marks where a device is in the tagout lifecycle.
type LockoutStatus string

const (
	// StatusLocked: equipment is tagged out and control commands are refused.
	StatusLocked LockoutStatus = "locked"
	// StatusReturnPending: field return checks passed, pre-operational
	// verification must still be re-run before service is restored.
	StatusReturnPending LockoutStatus = "return_pending"
)

// Phase selects at which gate a safety item must be confirmed.
type Phase string

const (
	PhasePlacement Phase = "placement"
	PhaseReturn    Phase = "return"
)

// SafetyCheck is one item on the isolation / return-to-service checklist.
type SafetyCheck struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Phase       Phase     `json:"phase"`
	Confirmed   bool      `json:"confirmed"`
	ConfirmedAt time.Time `json:"confirmed_at,omitempty"`
}

// SuspendedAction records an in-flight action that was frozen when the
// device was locked out, so it can be resumed (or reported) at clearance.
type SuspendedAction struct {
	Kind      string       `json:"kind"`
	Reference string       `json:"reference"`
	LineID    model.LineID `json:"line_id"`
	PausedAt  time.Time    `json:"paused_at"`
}

// Lockout is the field maintenance record for one device.
type Lockout struct {
	ID              uuid.UUID         `json:"id"`
	Device          DeviceRef         `json:"device"`
	Reason          string            `json:"reason"`
	Responsible     string            `json:"responsible"`
	Status          LockoutStatus     `json:"status"`
	AlarmID         uuid.UUID         `json:"alarm_id,omitempty"`
	Checks          []SafetyCheck     `json:"checks"`
	Suspended       []SuspendedAction `json:"suspended_actions,omitempty"`
	PlacedAt        time.Time         `json:"placed_at"`
	ReturnCheckedAt *time.Time        `json:"return_checked_at,omitempty"`
	RestoredAt      *time.Time        `json:"restored_at,omitempty"`
	VerifiedAt      *time.Time        `json:"verified_at,omitempty"`
}

// LockoutRecord is the journal payload for a placed lockout.
type LockoutRecord struct {
	Lockout Lockout `json:"lockout"`
}

// RestoreRecord is the journal payload for a completed return-to-service.
type RestoreRecord struct {
	LockoutID  uuid.UUID `json:"lockout_id"`
	Device     DeviceRef `json:"device"`
	Generation uint64    `json:"generation"`
	VerifiedAt time.Time `json:"verified_at"`
	RestoredAt time.Time `json:"restored_at"`
}

// Active reports whether the device still refuses control commands.
func (l Lockout) Active() bool {
	return l.RestoredAt == nil
}

// MissingChecks returns the checklist items for the given phase that have
// not been confirmed yet.
func (l Lockout) MissingChecks(phase Phase) []SafetyCheck {
	missing := make([]SafetyCheck, 0)
	for _, check := range l.Checks {
		if check.Phase == phase && !check.Confirmed {
			missing = append(missing, check)
		}
	}
	return missing
}

// LockoutBook holds the active maintenance records.
type LockoutBook struct {
	mu      sync.RWMutex
	records map[string]Lockout
}

// NewLockoutBook creates an empty book.
func NewLockoutBook() *LockoutBook {
	return &LockoutBook{records: make(map[string]Lockout)}
}

// Place registers a fresh lockout after validating that every placement
// checklist item has been signed off.
func (b *LockoutBook) Place(device DeviceRef, reason, responsible string, confirmedChecks []string, now time.Time) (Lockout, error) {
	if reason == "" || responsible == "" {
		return Lockout{}, errors.New("lockout reason and responsible person are required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if current, exists := b.records[device.ID]; exists && current.Active() {
		return Lockout{}, fmt.Errorf("device %s is already locked out", device.ID)
	}
	confirmed := make(map[string]bool, len(confirmedChecks))
	for _, id := range confirmedChecks {
		confirmed[id] = true
	}
	checks := StandardChecks(device.Kind)
	stamp := now.UTC()
	for index := range checks {
		if checks[index].Phase != PhasePlacement {
			continue
		}
		if !confirmed[checks[index].ID] {
			return Lockout{}, fmt.Errorf("placement check %s was not confirmed", checks[index].ID)
		}
		checks[index].Confirmed = true
		checks[index].ConfirmedAt = stamp
	}
	record := Lockout{
		ID: uuid.New(), Device: device, Reason: reason, Responsible: responsible,
		Status: StatusLocked, Checks: checks, PlacedAt: stamp,
	}
	b.records[device.ID] = record
	return record, nil
}

// PlaceConfirmed registers a lockout whose placement checklist has already
// been signed off (used by the field workflow and by journal replay).
func (b *LockoutBook) PlaceConfirmed(record Lockout) error {
	if record.ID == uuid.Nil || record.Device.ID == "" {
		return errors.New("lockout record is incomplete")
	}
	if len(record.MissingChecks(PhasePlacement)) > 0 {
		return errors.New("placement safety checklist is incomplete")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records[record.Device.ID] = record
	return nil
}

// ConfirmReturnChecks signs off the return-to-service checklist items.
func (b *LockoutBook) ConfirmReturnChecks(deviceID string, confirmed []string, now time.Time) (Lockout, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.records[deviceID]
	if !ok || !record.Active() {
		return Lockout{}, errors.New("active lockout is not found")
	}
	wanted := make(map[string]bool, len(confirmed))
	for _, id := range confirmed {
		wanted[id] = true
	}
	stamp := now.UTC()
	for index := range record.Checks {
		if record.Checks[index].Phase != PhaseReturn {
			continue
		}
		if !wanted[record.Checks[index].ID] {
			return Lockout{}, fmt.Errorf("return check %s was not confirmed", record.Checks[index].ID)
		}
		if !record.Checks[index].Confirmed {
			record.Checks[index].Confirmed = true
			record.Checks[index].ConfirmedAt = stamp
		}
	}
	record.Status = StatusReturnPending
	record.ReturnCheckedAt = &stamp
	b.records[deviceID] = record
	return record, nil
}

// MarkVerified records the successful pre-operational verification without
// clearing the lockout; Restore clears it afterwards.
func (b *LockoutBook) MarkVerified(deviceID string, now time.Time) (Lockout, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.records[deviceID]
	if !ok || !record.Active() {
		return Lockout{}, errors.New("active lockout is not found")
	}
	if record.Status != StatusReturnPending || len(record.MissingChecks(PhaseReturn)) > 0 {
		return Lockout{}, errors.New("return-to-service checklist must be confirmed before verification")
	}
	stamp := now.UTC()
	record.VerifiedAt = &stamp
	b.records[deviceID] = record
	return record, nil
}

// Restore removes the lockout after verification succeeded.
func (b *LockoutBook) Restore(deviceID string, now time.Time) (Lockout, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.records[deviceID]
	if !ok || !record.Active() {
		return Lockout{}, errors.New("active lockout is not found")
	}
	if record.VerifiedAt == nil {
		return Lockout{}, errors.New("pre-operational verification must be re-run before restoration")
	}
	stamp := now.UTC()
	record.RestoredAt = &stamp
	delete(b.records, deviceID)
	return record, nil
}

// AttachSuspended records an in-flight action that was frozen by the lockout.
func (b *LockoutBook) AttachSuspended(deviceID string, action SuspendedAction) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.records[deviceID]
	if !ok {
		return
	}
	record.Suspended = append(record.Suspended, action)
	b.records[deviceID] = record
}

// BindAlarm links the maintenance alarm that was raised with the lockout so
// it can be recovered automatically when service is restored.
func (b *LockoutBook) BindAlarm(deviceID string, alarmID uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.records[deviceID]
	if !ok {
		return
	}
	record.AlarmID = alarmID
	b.records[deviceID] = record
}

// Drop removes an active lockout. It is used during journal replay of a
// lockout-cleared event.
func (b *LockoutBook) Drop(deviceID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.records[deviceID]; !ok {
		return false
	}
	delete(b.records, deviceID)
	return true
}

// Blocked reports whether the device currently refuses commands.
func (b *LockoutBook) Blocked(deviceID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	record, ok := b.records[deviceID]
	return ok && record.Active()
}

// Get returns the active record for a device.
func (b *LockoutBook) Get(deviceID string) (Lockout, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	record, ok := b.records[deviceID]
	return record, ok
}

// Active returns every lockout that still blocks commands.
func (b *LockoutBook) Active() []Lockout {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Lockout, 0, len(b.records))
	for _, record := range b.records {
		if record.Active() {
			result = append(result, record)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Device.LineID != result[j].Device.LineID {
			return result[i].Device.LineID < result[j].Device.LineID
		}
		return result[i].Device.ID < result[j].Device.ID
	})
	return result
}

// StandardChecks returns the mandatory safety checklist for a device kind.
// Every item starts unconfirmed and must be signed at the matching gate.
func StandardChecks(kind model.DeviceKind) []SafetyCheck {
	common := []SafetyCheck{
		{ID: "isolated-power", Description: "动力电源已断开并上锁挂牌", Phase: PhasePlacement},
		{ID: "zero-energy", Description: "已验证零能量状态（停转/泄压/排空）", Phase: PhasePlacement},
		{ID: "guards-reset", Description: "防护罩、连接件复位且现场工具清点完毕", Phase: PhaseReturn},
		{ID: "power-restored", Description: "供电恢复且就地控制转换为远程允许", Phase: PhaseReturn},
	}
	specific := map[model.DeviceKind][]SafetyCheck{
		model.DeviceBlower: {
			{ID: "blower-inlet-valve", Description: "风机进出口风门关闭，管路已隔离", Phase: PhasePlacement},
			{ID: "blower-trial-run", Description: "风机点动校验：转向、振动与频率范围正常", Phase: PhaseReturn},
		},
		model.DevicePump: {
			{ID: "pump-suiction-valve", Description: "泵进出口阀门关闭，泵体泄压并排空", Phase: PhasePlacement},
			{ID: "pump-prime-trial", Description: "泵灌泵点动校验：转向、密封与流量正常", Phase: PhaseReturn},
		},
		model.DeviceValve: {
			{ID: "valve-line-blinded", Description: "阀前后管路已加装盲板/导淋并锁定", Phase: PhasePlacement},
			{ID: "valve-stroke-trial", Description: "阀门全行程动作校验：到位信号与泄漏正常", Phase: PhaseReturn},
		},
	}
	items := append([]SafetyCheck(nil), common...)
	items = append(items, specific[kind]...)
	return items
}
