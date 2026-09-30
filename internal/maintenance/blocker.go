package maintenance

import "errors"

// ErrDeviceLockedOut is returned when a control command targets a device
// that is locked out for field maintenance.
var ErrDeviceLockedOut = errors.New("device is locked out for maintenance")

// Blocker lets device controllers ask whether a device must refuse
// commands without depending on the maintenance book implementation.
type Blocker interface {
	DeviceBlocked(deviceID string) bool
}

// DeviceBlocked implements Blocker for the lockout book.
func (b *LockoutBook) DeviceBlocked(deviceID string) bool {
	return b.Blocked(deviceID)
}
