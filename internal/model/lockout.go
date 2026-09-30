package model

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// LockStatus describes the field-maintenance lifecycle of a device.
type LockStatus string

const (
	LockStatusTagged   LockStatus = "tagged"   // 现场检修封锁中（停用挂牌）
	LockStatusReleased LockStatus = "released" // 已复役
)

// Sentinel errors shared by the maintenance lockout workflow.
var (
	ErrDeviceLocked         = errors.New("device is locked out for field maintenance")
	ErrLockoutNotFound      = errors.New("device lockout is not found")
	ErrLockoutAlreadyActive = errors.New("device already has an active maintenance lockout")
	ErrChecksIncomplete     = errors.New("lockout safety checks are not all confirmed")
)

// SafetyCheck is a single mandatory safety verification item recorded on the tag.
type SafetyCheck struct {
	Code        string     `json:"code"`
	Label       string     `json:"label"`
	Confirmed   bool       `json:"confirmed"`
	ConfirmedBy string     `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
}

// LockoutRecord is the field maintenance tag of a blower, pump or discharge valve.
type LockoutRecord struct {
	ID               uuid.UUID     `json:"id"`
	DeviceID         string        `json:"device_id"`
	DeviceKind       DeviceKind    `json:"device_kind"`
	LineID           LineID        `json:"line_id"`
	Status           LockStatus    `json:"status"`
	Reason           string        `json:"reason"`
	Owner            string        `json:"owner"`
	TaggedBy         string        `json:"tagged_by,omitempty"`
	Checks           []SafetyCheck `json:"checks"`
	PreRunVerified   bool          `json:"pre_run_verified"`
	PreRunNote       string        `json:"pre_run_note,omitempty"`
	Generation       uint64        `json:"generation"`
	TaggedAt         time.Time     `json:"tagged_at"`
	AlarmID          uuid.UUID     `json:"alarm_id,omitempty"`
	CheckUpdatedAt   *time.Time    `json:"check_updated_at,omitempty"`
	PreRunVerifiedAt *time.Time    `json:"pre_run_verified_at,omitempty"`
	ReleasedAt       *time.Time    `json:"released_at,omitempty"`
	ReleasedBy       string        `json:"released_by,omitempty"`
}

// NewLockoutRecord opens a maintenance tag with the mandatory safety checklist.
func NewLockoutRecord(deviceID string, kind DeviceKind, lineID LineID, reason, owner, taggedBy string, extraChecks []SafetyCheck, generation uint64, now time.Time) (LockoutRecord, error) {
	if deviceID == "" || lineID == "" {
		return LockoutRecord{}, errors.New("lockout device identity is required")
	}
	if kind != DeviceBlower && kind != DevicePump && kind != DeviceValve {
		return LockoutRecord{}, fmt.Errorf("device kind %s cannot be locked out for field maintenance", kind)
	}
	if reason == "" {
		return LockoutRecord{}, errors.New("lockout decommission reason is required")
	}
	if owner == "" {
		return LockoutRecord{}, errors.New("lockout responsible person is required")
	}
	if generation == 0 {
		return LockoutRecord{}, errors.New("lockout generation is required")
	}
	checks := StandardSafetyChecks(kind)
	for _, check := range extraChecks {
		if check.Code == "" || check.Label == "" {
			return LockoutRecord{}, errors.New("custom safety check is incomplete")
		}
		if _, exists := findCheck(checks, check.Code); exists {
			continue
		}
		check.Confirmed = false
		check.ConfirmedBy = ""
		check.ConfirmedAt = nil
		checks = append(checks, check)
	}
	return LockoutRecord{
		ID:         uuid.New(),
		DeviceID:   deviceID,
		DeviceKind: kind,
		LineID:     lineID,
		Status:     LockStatusTagged,
		Reason:     reason,
		Owner:      owner,
		TaggedBy:   taggedBy,
		Checks:     checks,
		Generation: generation,
		TaggedAt:   now.UTC(),
	}, nil
}

// Active reports whether the device is currently locked out.
func (r LockoutRecord) Active() bool {
	return r.Status == LockStatusTagged
}

// ChecksConfirmed reports whether every mandatory safety item is signed off.
func (r LockoutRecord) ChecksConfirmed() bool {
	if len(r.Checks) == 0 {
		return false
	}
	for _, check := range r.Checks {
		if !check.Confirmed {
			return false
		}
	}
	return true
}

// ConfirmCheck signs off a single safety checklist item.
func (r *LockoutRecord) ConfirmCheck(code, confirmedBy string, now time.Time) error {
	if !r.Active() {
		return errors.New("lockout is not active")
	}
	index, ok := findCheck(r.Checks, code)
	if !ok {
		return fmt.Errorf("safety check %s is not on the lockout checklist", code)
	}
	if confirmedBy == "" {
		return errors.New("safety check confirmation requires a signer")
	}
	stamp := now.UTC()
	r.Checks[index].Confirmed = true
	r.Checks[index].ConfirmedBy = confirmedBy
	r.Checks[index].ConfirmedAt = &stamp
	r.CheckUpdatedAt = &stamp
	return nil
}

// MarkPreRunVerified records the pre-commissioning verification outcome.
func (r *LockoutRecord) MarkPreRunVerified(note string, now time.Time) error {
	if !r.Active() {
		return errors.New("lockout is not active")
	}
	if !r.ChecksConfirmed() {
		return ErrChecksIncomplete
	}
	stamp := now.UTC()
	r.PreRunVerified = true
	r.PreRunNote = note
	r.PreRunVerifiedAt = &stamp
	return nil
}

// Release closes the tag after every check is confirmed and pre-run verification passed.
func (r *LockoutRecord) Release(releasedBy string, now time.Time) error {
	if !r.Active() {
		return errors.New("lockout is not active")
	}
	if !r.ChecksConfirmed() {
		return ErrChecksIncomplete
	}
	if !r.PreRunVerified {
		return errors.New("pre-commissioning verification is required before release")
	}
	if releasedBy == "" {
		return errors.New("lockout release requires an operator")
	}
	stamp := now.UTC()
	r.Status = LockStatusReleased
	r.ReleasedAt = &stamp
	r.ReleasedBy = releasedBy
	return nil
}

func findCheck(checks []SafetyCheck, code string) (int, bool) {
	for index, check := range checks {
		if check.Code == code {
			return index, true
		}
	}
	return 0, false
}

// StandardSafetyChecks returns the mandatory field-maintenance checklist for a device kind.
func StandardSafetyChecks(kind DeviceKind) []SafetyCheck {
	common := []SafetyCheck{
		{Code: "energy_isolated", Label: "动力能源已隔离并上锁挂牌（LOTO）"},
		{Code: "zero_motion", Label: "设备已完全停止，转动部件静止并验明无残压/残动能"},
		{Code: "site_warning", Label: "现场已设置检修警示，无关人员无法误启动"},
	}
	switch kind {
	case DeviceBlower:
		return append([]SafetyCheck{
			{Code: "duct_damper", Label: "进出风管路挡板已关闭并固定"},
			{Code: "space_ventilated", Label: "风机房通风合格、有限空间气体检测达标"},
		}, common...)
	case DevicePump:
		return append([]SafetyCheck{
			{Code: "suction_discharge_closed", Label: "吸入/排出侧阀门已关闭"},
			{Code: "depressurized_drained", Label: "泵体已泄压并排空残液"},
		}, common...)
	case DeviceValve:
		return append([]SafetyCheck{
			{Code: "line_isolated", Label: "阀门上下游管线已隔离"},
			{Code: "actuator_isolated", Label: "电动/气动执行机构能源已隔离"},
			{Code: "depressurized_drained", Label: "阀腔已泄压并排空"},
		}, common...)
	default:
		return common
	}
}
