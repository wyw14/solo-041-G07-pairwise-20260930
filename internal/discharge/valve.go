package discharge

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type Valve struct {
	ID        string           `json:"id"`
	LineID    model.LineID     `json:"line_id"`
	Kind      model.DeviceKind `json:"kind"`
	Open      bool             `json:"open"`
	Locked    bool             `json:"locked"`
	PermitID  uuid.UUID        `json:"permit_id,omitempty"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// SafetyGate reports whether a device is locked out for field maintenance.
type SafetyGate interface {
	IsLocked(deviceID string) bool
}

type ValveController struct {
	mu     sync.RWMutex
	valves map[model.LineID]Valve
	gate   SafetyGate
}

// ValveID is the canonical device id of a line's discharge valve.
func ValveID(lineID model.LineID) string {
	return "valve-" + string(lineID)
}

func NewValveController(lines []model.LineID) (*ValveController, error) {
	if len(lines) == 0 {
		return nil, errors.New("discharge lines are required")
	}
	controller := &ValveController{valves: make(map[model.LineID]Valve, len(lines))}
	for _, lineID := range lines {
		if lineID == "" {
			return nil, errors.New("discharge line is empty")
		}
		controller.valves[lineID] = Valve{ID: ValveID(lineID), LineID: lineID, Kind: model.DeviceValve}
	}
	return controller, nil
}

// BindSafetyGate attaches the maintenance lockout registry.
func (c *ValveController) BindSafetyGate(gate SafetyGate) {
	c.mu.Lock()
	c.gate = gate
	c.mu.Unlock()
}

func (c *ValveController) Open(lineID model.LineID, permitID uuid.UUID, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	valve, ok := c.valves[lineID]
	if !ok || permitID == uuid.Nil {
		return errors.New("discharge valve target is invalid")
	}
	// 封锁中的排水阀拒绝开启，即便持有有效排放许可。
	if c.gate != nil && c.gate.IsLocked(valve.ID) {
		return model.ErrDeviceLocked
	}
	valve.Open = true
	valve.PermitID = permitID
	valve.UpdatedAt = now.UTC()
	c.valves[lineID] = valve
	return nil
}

func (c *ValveController) Hold(lineID model.LineID, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	valve, ok := c.valves[lineID]
	if !ok {
		return errors.New("discharge valve is not found")
	}
	valve.Open = false
	valve.PermitID = uuid.Nil
	valve.UpdatedAt = now.UTC()
	c.valves[lineID] = valve
	return nil
}

// Isolate forces the valve closed and locked while maintenance owns the line.
func (c *ValveController) Isolate(lineID model.LineID, now time.Time) (Valve, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	valve, ok := c.valves[lineID]
	if !ok {
		return Valve{}, errors.New("discharge valve is not found")
	}
	valve.Open = false
	valve.PermitID = uuid.Nil
	valve.Locked = true
	valve.UpdatedAt = now.UTC()
	c.valves[lineID] = valve
	return valve, nil
}

// RestoreService clears the locked flag; the valve remains closed until a new
// permit and pre-run verification reopen it.
func (c *ValveController) RestoreService(lineID model.LineID, now time.Time) (Valve, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	valve, ok := c.valves[lineID]
	if !ok {
		return Valve{}, errors.New("discharge valve is not found")
	}
	valve.Locked = false
	valve.UpdatedAt = now.UTC()
	c.valves[lineID] = valve
	return valve, nil
}

func (c *ValveController) Snapshot() []Valve {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]Valve, 0, len(c.valves))
	for _, valve := range c.valves {
		result = append(result, valve)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LineID < result[j].LineID })
	return result
}
