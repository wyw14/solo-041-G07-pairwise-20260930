package journal

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type Compensation struct {
	ID         uuid.UUID    `json:"id"`
	LineID     model.LineID `json:"line_id"`
	SequenceID uuid.UUID    `json:"sequence_id"`
	Generation uint64       `json:"generation"`
	DeviceID   string       `json:"device_id"`
	Action     string       `json:"action"`
	DueAt      time.Time    `json:"due_at"`
}

func NewCompensation(lineID model.LineID, sequenceID uuid.UUID, generation uint64, deviceID, action string, dueAt time.Time) (Compensation, error) {
	if lineID == "" || sequenceID == uuid.Nil || generation == 0 || deviceID == "" || action == "" {
		return Compensation{}, errors.New("compensation identity is incomplete")
	}
	return Compensation{
		ID: uuid.New(), LineID: lineID, SequenceID: sequenceID, Generation: generation,
		DeviceID: deviceID, Action: action, DueAt: dueAt.UTC(),
	}, nil
}

type CompensationExecutor func(context.Context, Compensation) error

type CompensationQueue struct {
	mu      sync.Mutex
	pending []Compensation
}

func NewCompensationQueue() *CompensationQueue {
	return &CompensationQueue{pending: make([]Compensation, 0, 8)}
}

func (q *CompensationQueue) Schedule(item Compensation) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, item)
	sort.SliceStable(q.pending, func(i, j int) bool { return q.pending[i].DueAt.Before(q.pending[j].DueAt) })
}

func (q *CompensationQueue) ExecuteDue(ctx context.Context, now time.Time, execute CompensationExecutor) (int, error) {
	if execute == nil {
		return 0, errors.New("compensation executor is required")
	}
	q.mu.Lock()
	due := make([]Compensation, 0, len(q.pending))
	remaining := q.pending[:0]
	for _, item := range q.pending {
		if !item.DueAt.After(now) {
			due = append(due, item)
		} else {
			remaining = append(remaining, item)
		}
	}
	q.pending = remaining
	q.mu.Unlock()
	for index, item := range due {
		if err := execute(ctx, item); err != nil {
			q.mu.Lock()
			q.pending = append(due[index:], q.pending...)
			q.mu.Unlock()
			return index, err
		}
	}
	return len(due), nil
}

func (q *CompensationQueue) Pending() []Compensation {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Compensation(nil), q.pending...)
}

// SuspendDevice pulls every pending compensation targeting a device out of
// the queue so maintenance work is not disturbed. Return the items to
// Reschedule once the device is back in service.
func (q *CompensationQueue) SuspendDevice(deviceID string) []Compensation {
	q.mu.Lock()
	defer q.mu.Unlock()
	suspended := make([]Compensation, 0)
	remaining := q.pending[:0]
	for _, item := range q.pending {
		if item.DeviceID == deviceID {
			suspended = append(suspended, item)
		} else {
			remaining = append(remaining, item)
		}
	}
	q.pending = remaining
	return suspended
}

// Reschedule returns previously suspended compensations to the queue.
func (q *CompensationQueue) Reschedule(items []Compensation) {
	if len(items) == 0 {
		return
	}
	q.mu.Lock()
	q.pending = append(q.pending, items...)
	sort.SliceStable(q.pending, func(i, j int) bool { return q.pending[i].DueAt.Before(q.pending[j].DueAt) })
	q.mu.Unlock()
}
