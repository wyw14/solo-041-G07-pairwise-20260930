package discharge

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/maintenance"
	"github.com/wyw14/cry-97/internal/model"
)

// ValveID returns the catalog device id of a line's discharge valve.
func ValveID(lineID model.LineID) string {
	return "valve-" + string(lineID)
}

type Valve struct {
	ID        string           `json:"id"`
	LineID    model.LineID     `json:"line_id"`
	Kind      model.DeviceKind `json:"kind"`
	Open      bool             `json:"open"`
	PermitID  uuid.UUID        `json:"permit_id,omitempty"`
	UpdatedAt time.Time        `json:"updated_at"`
}

type ValveController struct {
	mu      sync.RWMutex
	valves  map[model.LineID]Valve
	blocker maintenance.Blocker
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

// BindBlocker makes the controller reject opens of locked-out valves.
func (c *ValveController) BindBlocker(blocker maintenance.Blocker) {
	c.mu.Lock()
	c.blocker = blocker
	c.mu.Unlock()
}

func (c *ValveController) Open(lineID model.LineID, permitID uuid.UUID, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	valve, ok := c.valves[lineID]
	if !ok || permitID == uuid.Nil {
		return errors.New("discharge valve target is invalid")
	}
	if c.blocker != nil && c.blocker.DeviceBlocked(valve.ID) {
		return maintenance.ErrDeviceLockedOut
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

// MaintenanceClose forces a discharge valve shut on lockout, revoking any
// permit reference the field work invalidates.
func (c *ValveController) MaintenanceClose(lineID model.LineID, now time.Time) error {
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
