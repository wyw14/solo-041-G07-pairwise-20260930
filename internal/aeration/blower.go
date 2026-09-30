package aeration

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-97/internal/maintenance"
	"github.com/wyw14/cry-97/internal/model"
)

type Blower struct {
	ID        string    `json:"id"`
	BasinID   string    `json:"basin_id"`
	Frequency float64   `json:"frequency"`
	Running   bool      `json:"running"`
	UpdatedAt time.Time `json:"updated_at"`
}

type BlowerFleet struct {
	mu      sync.RWMutex
	blowers map[string]Blower
	blocker maintenance.Blocker
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

// BindBlocker makes the fleet reject commands for locked-out blowers.
func (f *BlowerFleet) BindBlocker(blocker maintenance.Blocker) {
	f.mu.Lock()
	f.blocker = blocker
	f.mu.Unlock()
}

func (f *BlowerFleet) Apply(command model.DeviceCommand, now time.Time) (Blower, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	blower, ok := f.blowers[command.DeviceID]
	if !ok || command.Kind != model.DeviceBlower {
		return Blower{}, errors.New("blower command target is unknown")
	}
	if f.blocker != nil && f.blocker.DeviceBlocked(command.DeviceID) {
		return Blower{}, maintenance.ErrDeviceLockedOut
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

// Stop shuts a blower down on lockout regardless of command generation.
func (f *BlowerFleet) Stop(deviceID string, now time.Time) (Blower, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	blower, ok := f.blowers[deviceID]
	if !ok {
		return Blower{}, errors.New("blower command target is unknown")
	}
	blower.Frequency = 0
	blower.Running = false
	blower.UpdatedAt = now.UTC()
	f.blowers[deviceID] = blower
	return blower, nil
}

// VerifyReady performs the pre-operational check for a blower that is still
// tagged out: it bypasses the command gate and confirms the unit is latched
// at a safe stop before clearance re-enables control commands.
func (f *BlowerFleet) VerifyReady(deviceID string, now time.Time) (Blower, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	blower, ok := f.blowers[deviceID]
	if !ok {
		return Blower{}, errors.New("blower command target is unknown")
	}
	if blower.Running || blower.Frequency != 0 {
		return Blower{}, errors.New("blower is not at safe stop")
	}
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
