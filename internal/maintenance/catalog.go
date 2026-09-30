// Package maintenance implements field maintenance lockout/tagout and
// return-to-service workflows for rotating and actuating equipment.
package maintenance

import (
	"errors"
	"sort"
	"sync"

	"github.com/wyw14/cry-97/internal/model"
)

// DeviceRef identifies a maintainable piece of equipment and the process
// line whose journal and state it belongs to.
type DeviceRef struct {
	ID     string           `json:"id"`
	Kind   model.DeviceKind `json:"kind"`
	LineID model.LineID     `json:"line_id"`
}

// Catalog is the authoritative registry of blowers, pumps and valves that
// can be locked out for field maintenance.
type Catalog struct {
	mu     sync.RWMutex
	device map[string]DeviceRef
}

// NewCatalog builds a catalog and rejects duplicate or incomplete entries.
func NewCatalog(refs []DeviceRef) (*Catalog, error) {
	catalog := &Catalog{device: make(map[string]DeviceRef, len(refs))}
	for _, ref := range refs {
		switch ref.Kind {
		case model.DeviceBlower, model.DevicePump, model.DeviceValve:
		default:
			return nil, errors.New("maintenance catalog only covers blowers, pumps and valves")
		}
		if ref.ID == "" || ref.LineID == "" {
			return nil, errors.New("maintenance device identity is incomplete")
		}
		if _, exists := catalog.device[ref.ID]; exists {
			return nil, errors.New("maintenance device is registered twice")
		}
		catalog.device[ref.ID] = ref
	}
	if len(catalog.device) == 0 {
		return nil, errors.New("maintenance catalog is empty")
	}
	return catalog, nil
}

// Lookup returns the registered device.
func (c *Catalog) Lookup(deviceID string) (DeviceRef, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ref, ok := c.device[deviceID]
	return ref, ok
}

// All returns catalog entries ordered by device id.
func (c *Catalog) All() []DeviceRef {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]DeviceRef, 0, len(c.device))
	for _, ref := range c.device {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].LineID != result[j].LineID {
			return result[i].LineID < result[j].LineID
		}
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return result[i].ID < result[j].ID
	})
	return result
}
