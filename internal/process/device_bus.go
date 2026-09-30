package process

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/wyw14/cry-97/internal/maintenance"
	"github.com/wyw14/cry-97/internal/model"
)

type DeviceBus struct {
	mu       sync.RWMutex
	commands map[string]model.DeviceCommand
	blocker  maintenance.Blocker
}

func NewDeviceBus() *DeviceBus {
	return &DeviceBus{commands: make(map[string]model.DeviceCommand)}
}

// BindBlocker wires the maintenance lockout book so locked-out devices
// refuse published control commands.
func (b *DeviceBus) BindBlocker(blocker maintenance.Blocker) {
	b.mu.Lock()
	b.blocker = blocker
	b.mu.Unlock()
}

func (b *DeviceBus) Publish(ctx context.Context, command model.DeviceCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if command.DeviceID == "" || command.Generation == 0 {
		return errors.New("device command is incomplete")
	}
	b.mu.Lock()
	if b.blocker != nil && b.blocker.DeviceBlocked(command.DeviceID) {
		b.mu.Unlock()
		return maintenance.ErrDeviceLockedOut
	}
	current, exists := b.commands[command.DeviceID]
	if exists && command.Generation < current.Generation {
		b.mu.Unlock()
		return errors.New("device command generation is stale")
	}
	b.commands[command.DeviceID] = command
	b.mu.Unlock()
	return nil
}

func (b *DeviceBus) All() []model.DeviceCommand {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]model.DeviceCommand, 0, len(b.commands))
	for _, command := range b.commands {
		result = append(result, command)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IssuedAt.Before(result[j].IssuedAt) })
	return result
}

func (b *DeviceBus) StopLine(lineID model.LineID, generation uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, command := range b.commands {
		if command.LineID != lineID {
			continue
		}
		command.Generation = generation
		command.Action = "stop"
		command.Setpoint = 0
		b.commands[id] = command
	}
}

// StopDevice forces a stop command onto a single device. It is used when
// the device is locked out for field maintenance.
func (b *DeviceBus) StopDevice(deviceID string, generation uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	command, exists := b.commands[deviceID]
	if !exists {
		return
	}
	command.Generation = generation
	command.Action = "stop"
	command.Setpoint = 0
	b.commands[deviceID] = command
}
