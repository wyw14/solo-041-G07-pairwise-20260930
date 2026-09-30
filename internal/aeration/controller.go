package aeration

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/sensor"
)

type Controller struct {
	decoder *sensor.BatchDecoder
	windows *WindowBook
	blowers *BlowerFleet
}

func NewController(decoder *sensor.BatchDecoder, windows *WindowBook, blowers *BlowerFleet) (*Controller, error) {
	if decoder == nil || windows == nil || blowers == nil {
		return nil, errors.New("aeration controller dependencies are required")
	}
	return &Controller{decoder: decoder, windows: windows, blowers: blowers}, nil
}

func (c *Controller) SubmitWindow(ctx context.Context, lineID model.LineID, basinID string, payload []byte, generation uint64, now time.Time) (Window, *model.DeviceCommand, error) {
	if err := ctx.Err(); err != nil {
		return Window{}, nil, err
	}
	values, err := c.decoder.Decode(payload)
	if err != nil {
		return Window{}, nil, err
	}
	window, err := c.windows.Store(basinID, values)
	if err != nil {
		return Window{}, nil, err
	}
	if !window.Stable {
		return window, nil, nil
	}
	target := 36.0
	if window.Average < 2.0 {
		target = 48
	} else if window.Average > 3.5 {
		target = 28
	}
	command, err := model.NewDeviceCommand(lineID, uuid.New(), "blower-"+basinID, model.DeviceBlower, generation, "set", now)
	if err != nil {
		return Window{}, nil, err
	}
	command.Setpoint = target
	if _, err := c.blowers.Apply(command, now); err != nil {
		return Window{}, nil, err
	}
	return window, &command, nil
}

func (c *Controller) Windows() []Window {
	return c.windows.All()
}

func (c *Controller) Blowers() []Blower {
	return c.blowers.Snapshot()
}

// IsolateBlower forces a blower to a stopped, locked-out state at tag time.
func (c *Controller) IsolateBlower(deviceID string, now time.Time) (Blower, error) {
	return c.blowers.Isolate(deviceID, now)
}

// RestoreBlower clears the locked flag after the blower is returned to service.
func (c *Controller) RestoreBlower(deviceID string, now time.Time) (Blower, error) {
	return c.blowers.RestoreService(deviceID, now)
}

// PreRunBlocker returns a non-empty reason if a blower cannot pass pre-run
// verification (still running or its lockout state was lost).
func (c *Controller) PreRunBlocker(deviceID string) string {
	for _, blower := range c.blowers.Snapshot() {
		if blower.ID != deviceID {
			continue
		}
		if blower.Running {
			return "blower is still running before pre-run verification"
		}
		if !blower.Locked {
			return "blower lockout state was lost"
		}
		return ""
	}
	return "blower is not found"
}
