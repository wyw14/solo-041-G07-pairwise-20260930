package sludge

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type Pump struct {
	ID              string           `json:"id"`
	LineID          model.LineID     `json:"line_id"`
	Kind            model.DeviceKind `json:"kind"`
	Running         bool             `json:"running"`
	Primed          bool             `json:"primed"`
	Locked          bool             `json:"locked"`
	Flow            float64          `json:"flow"`
	OwnerSequence   uuid.UUID        `json:"owner_sequence,omitempty"`
	OwnerGeneration uint64           `json:"owner_generation"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// SafetyGate reports whether a device is locked out for field maintenance.
type SafetyGate interface {
	IsLocked(deviceID string) bool
}

type PumpController struct {
	mu    sync.RWMutex
	pumps map[string]Pump
	gate  SafetyGate
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

// BindSafetyGate attaches the maintenance lockout registry.
func (c *PumpController) BindSafetyGate(gate SafetyGate) {
	c.mu.Lock()
	c.gate = gate
	c.mu.Unlock()
}

// blockedLocked reports the locked state while the controller write lock is held.
func (c *PumpController) blockedLocked(pumpID string) bool {
	return c.gate != nil && c.gate.IsLocked(pumpID)
}

func (c *PumpController) Assign(pumpID string, sequenceID uuid.UUID, generation uint64, now time.Time) (Pump, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pump, ok := c.pumps[pumpID]
	if !ok || sequenceID == uuid.Nil || generation == 0 {
		return Pump{}, errors.New("pump assignment is invalid")
	}
	if c.blockedLocked(pumpID) {
		return Pump{}, model.ErrDeviceLocked
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
		return Pump{}, model.ErrDeviceLocked
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
	// 停止命令属于安全动作，即使泵已封锁也允许执行；但封锁期间所有权可能已被
	// 挂起保留，因此不再校验所有权代际，只接受“匹配”或“无主/已挂起”的停机。
	if !pump.Locked && (pump.OwnerSequence != sequenceID || pump.OwnerGeneration != generation) {
		return Pump{}, errors.New("pump ownership changed before stop")
	}
	pump.Running = false
	pump.Flow = 0
	pump.UpdatedAt = now.UTC()
	c.pumps[pumpID] = pump
	return pump, nil
}

// Isolate forces a pump to a stopped, locked-out state at tag time while keeping
// its owning sequence so the in-flight action can be resumed after release.
func (c *PumpController) Isolate(pumpID string, now time.Time) (Pump, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pump, ok := c.pumps[pumpID]
	if !ok {
		return Pump{}, errors.New("return pump is not found")
	}
	pump.Running = false
	pump.Primed = false
	pump.Flow = 0
	pump.Locked = true
	pump.UpdatedAt = now.UTC()
	c.pumps[pumpID] = pump
	return pump, nil
}

// RestoreService clears the locked flag; the pump stays stopped until its
// suspended owner re-confirms flow through the pre-run check.
func (c *PumpController) RestoreService(pumpID string, now time.Time) (Pump, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pump, ok := c.pumps[pumpID]
	if !ok {
		return Pump{}, errors.New("return pump is not found")
	}
	pump.Locked = false
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
