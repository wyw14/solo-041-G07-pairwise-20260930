package sludge

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/maintenance"
	"github.com/wyw14/cry-97/internal/model"
)

type Pump struct {
	ID              string           `json:"id"`
	LineID          model.LineID     `json:"line_id"`
	Kind            model.DeviceKind `json:"kind"`
	Running         bool             `json:"running"`
	Primed          bool             `json:"primed"`
	Flow            float64          `json:"flow"`
	OwnerSequence   uuid.UUID        `json:"owner_sequence,omitempty"`
	OwnerGeneration uint64           `json:"owner_generation"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

type PumpController struct {
	mu      sync.RWMutex
	pumps   map[string]Pump
	blocker maintenance.Blocker
}

func NewPumpController(pumps []Pump) (*PumpController, error) {
	if len(pumps) < 2 {
		return nil, errors.New("at least two return pumps are required")
	}
	controller := &PumpController{pumps: make(map[string]Pump, len(pumps))}
	for _, pump := range pumps {
		if pump.ID == "" || pump.LineID == "" {
			return nil, errors.New("pump identity is incomplete")
		}
		controller.pumps[pump.ID] = pump
	}
	return controller, nil
}

// BindBlocker makes the controller reject commands for locked-out pumps.
func (c *PumpController) BindBlocker(blocker maintenance.Blocker) {
	c.mu.Lock()
	c.blocker = blocker
	c.mu.Unlock()
}

func (c *PumpController) blockedLocked(deviceID string) bool {
	return c.blocker != nil && c.blocker.DeviceBlocked(deviceID)
}

func (c *PumpController) Assign(pumpID string, sequenceID uuid.UUID, generation uint64, now time.Time) (Pump, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pump, ok := c.pumps[pumpID]
	if !ok || sequenceID == uuid.Nil || generation == 0 {
		return Pump{}, errors.New("pump assignment is invalid")
	}
	if c.blockedLocked(pumpID) {
		return Pump{}, maintenance.ErrDeviceLockedOut
	}
	pump.OwnerSequence = sequenceID
	pump.OwnerGeneration = generation
	pump.Primed = false
	pump.Running = false
	pump.Flow = 0
	pump.UpdatedAt = now.UTC()
	c.pumps[pumpID] = pump
	return pump, nil
}

func (c *PumpController) Ready(pumpID string, sequenceID uuid.UUID, generation uint64, flow float64, now time.Time) (Pump, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pump, ok := c.pumps[pumpID]
	if !ok {
		return Pump{}, errors.New("return pump is not found")
	}
	if c.blockedLocked(pumpID) {
		return Pump{}, maintenance.ErrDeviceLockedOut
	}
	if pump.OwnerSequence != sequenceID || pump.OwnerGeneration != generation {
		return Pump{}, errors.New("pump ready confirmation is stale")
	}
	if flow <= 0 {
		return Pump{}, errors.New("pump ready confirmation has no flow")
	}
	pump.Primed = true
	pump.Running = true
	pump.Flow = flow
	pump.UpdatedAt = now.UTC()
	c.pumps[pumpID] = pump
	return pump, nil
}

func (c *PumpController) StopOwned(pumpID string, sequenceID uuid.UUID, generation uint64, now time.Time) (Pump, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pump, ok := c.pumps[pumpID]
	if !ok {
		return Pump{}, errors.New("return pump is not found")
	}
	if pump.OwnerSequence != sequenceID || pump.OwnerGeneration != generation {
		return Pump{}, errors.New("pump ownership changed before stop")
	}
	pump.Running = false
	pump.Flow = 0
	pump.UpdatedAt = now.UTC()
	c.pumps[pumpID] = pump
	return pump, nil
}

// MaintenanceStop forces a pump to stop when it is locked out, bypassing
// ownership checks because the field tagout supersedes in-flight actions.
func (c *PumpController) MaintenanceStop(pumpID string, now time.Time) (Pump, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pump, ok := c.pumps[pumpID]
	if !ok {
		return Pump{}, errors.New("return pump is not found")
	}
	pump.Running = false
	pump.Primed = false
	pump.Flow = 0
	pump.UpdatedAt = now.UTC()
	c.pumps[pumpID] = pump
	return pump, nil
}

func (c *PumpController) Snapshot() []Pump {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]Pump, 0, len(c.pumps))
	for _, pump := range c.pumps {
		result = append(result, pump)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
