package aeration

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

type Blower struct {
	ID        string    `json:"id"`
	BasinID   string    `json:"basin_id"`
	Frequency float64   `json:"frequency"`
	Running   bool      `json:"running"`
	Locked    bool      `json:"locked"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SafetyGate reports whether a device is locked out for field maintenance.
type SafetyGate interface {
	IsLocked(deviceID string) bool
}

type BlowerFleet struct {
	mu      sync.RWMutex
	blowers map[string]Blower
	gate    SafetyGate
}

func NewBlowerFleet(blowers []Blower) (*BlowerFleet, error) {
	if len(blowers) == 0 {
		return nil, errors.New("at least one blower is required")
	}
	fleet := &BlowerFleet{blowers: make(map[string]Blower, len(blowers))}
	for _, blower := range blowers {
		if blower.ID == "" || blower.BasinID == "" {
			return nil, errors.New("blower identity is required")
		}
		fleet.blowers[blower.ID] = blower
	}
	return fleet, nil
}

// BindSafetyGate attaches the maintenance lockout registry.
func (f *BlowerFleet) BindSafetyGate(gate SafetyGate) {
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
}

func (f *BlowerFleet) Apply(command model.DeviceCommand, now time.Time) (Blower, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	blower, ok := f.blowers[command.DeviceID]
	if !ok || command.Kind != model.DeviceBlower {
		return Blower{}, errors.New("blower command target is unknown")
	}
	// 封锁中的风机拒绝一切启动/调节命令；stop（零给定）始终放行，
	// 保证挂牌时在途曝气能被驱至安全停机状态。
	if f.gate != nil && f.gate.IsLocked(command.DeviceID) && command.Action != "stop" && command.Setpoint > 0 {
		return Blower{}, model.ErrDeviceLocked
	}
	if command.Setpoint < 0 || command.Setpoint > 60 {
		return Blower{}, errors.New("blower frequency is outside operating range")
	}
	blower.Frequency = command.Setpoint
	blower.Running = command.Setpoint > 0
	blower.UpdatedAt = now.UTC()
	f.blowers[blower.ID] = blower
	return blower, nil
}

// Isolate forces a blower to a stopped, locked-out state when a tag is raised.
func (f *BlowerFleet) Isolate(deviceID string, now time.Time) (Blower, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	blower, ok := f.blowers[deviceID]
	if !ok {
		return Blower{}, errors.New("blower is not found")
	}
	blower.Running = false
	blower.Frequency = 0
	blower.Locked = true
	blower.UpdatedAt = now.UTC()
	f.blowers[deviceID] = blower
	return blower, nil
}

// RestoreService clears the locked flag after the blower is returned to service.
func (f *BlowerFleet) RestoreService(deviceID string, now time.Time) (Blower, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	blower, ok := f.blowers[deviceID]
	if !ok {
		return Blower{}, errors.New("blower is not found")
	}
	blower.Locked = false
	blower.UpdatedAt = now.UTC()
	f.blowers[deviceID] = blower
	return blower, nil
}

func (f *BlowerFleet) Snapshot() []Blower {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make([]Blower, 0, len(f.blowers))
	for _, blower := range f.blowers {
		result = append(result, blower)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
