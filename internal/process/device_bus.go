package process

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// CommandGate reports whether a device is locked out for field maintenance.
type CommandGate interface {
	IsLocked(deviceID string) bool
}

type DeviceBus struct {
	mu       sync.RWMutex
	commands map[string]model.DeviceCommand
	gate     CommandGate
}

func NewDeviceBus() *DeviceBus {
	return &DeviceBus{commands: make(map[string]model.DeviceCommand)}
}

// BindSafetyGate attaches the maintenance lockout registry.
func (b *DeviceBus) BindSafetyGate(gate CommandGate) {
	b.mu.Lock()
	b.gate = gate
	b.mu.Unlock()
}

func (b *DeviceBus) Publish(ctx context.Context, command model.DeviceCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if command.DeviceID == "" || command.Generation == 0 {
		return errors.New("device command is incomplete")
	}
	// 封锁期间拒绝该设备的一切控制命令；stop 作为安全停机动作放行。
	if b.gate != nil && b.gate.IsLocked(command.DeviceID) && command.Action != "stop" {
		return model.ErrDeviceLocked
	}
	b.mu.Lock()
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

// StopDevice overwrites the current command of one locked-out device with a
// stop command, driving any in-flight action to the safe state.
func (b *DeviceBus) StopDevice(lineID model.LineID, deviceID string, kind model.DeviceKind, generation uint64, now time.Time) (model.DeviceCommand, error) {
	command, err := model.NewDeviceCommand(lineID, uuid.New(), deviceID, kind, generation, "stop", now)
	if err != nil {
		return model.DeviceCommand{}, err
	}
	b.mu.Lock()
	b.commands[deviceID] = command
	b.mu.Unlock()
	return command, nil
}
