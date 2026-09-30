package sludge

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/journal"
	"github.com/wyw14/cry-97/internal/model"
)

type SequenceStatus string

const (
	SequenceSwitching SequenceStatus = "switching"
	SequenceRunning   SequenceStatus = "running"
	SequenceSuspended SequenceStatus = "suspended"
	SequenceFailed    SequenceStatus = "failed"
)

type Sequence struct {
	ID         uuid.UUID      `json:"id"`
	LineID     model.LineID   `json:"line_id"`
	Number     uint64         `json:"number"`
	Generation uint64         `json:"generation"`
	PumpID     string         `json:"pump_id"`
	Status     SequenceStatus `json:"status"`
	StartedAt  time.Time      `json:"started_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

type SequenceBook struct {
	mu     sync.RWMutex
	byLine map[model.LineID]Sequence
	pumps  *PumpController
	queue  *journal.CompensationQueue
}

func NewSequenceBook(pumps *PumpController, queue *journal.CompensationQueue) (*SequenceBook, error) {
	if pumps == nil || queue == nil {
		return nil, errors.New("sludge sequence dependencies are required")
	}
	return &SequenceBook{byLine: make(map[model.LineID]Sequence), pumps: pumps, queue: queue}, nil
}

func (b *SequenceBook) Start(lineID model.LineID, pumpID string, number, generation uint64, now time.Time) (Sequence, error) {
	if lineID == "" || pumpID == "" || number == 0 || generation == 0 {
		return Sequence{}, errors.New("sludge sequence is invalid")
	}
	sequence := Sequence{
		ID: uuid.New(), LineID: lineID, Number: number, Generation: generation,
		PumpID: pumpID, Status: SequenceSwitching, StartedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	if _, err := b.pumps.Assign(pumpID, sequence.ID, generation, now); err != nil {
		return Sequence{}, err
	}
	b.mu.Lock()
	b.byLine[lineID] = sequence
	b.mu.Unlock()
	return sequence, nil
}

func (b *SequenceBook) ConfirmFlow(lineID model.LineID, sequenceID uuid.UUID, generation uint64, flow float64, now time.Time) (Sequence, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sequence, ok := b.byLine[lineID]
	if !ok || sequence.ID != sequenceID || sequence.Generation != generation {
		return Sequence{}, errors.New("sludge sequence confirmation is stale")
	}
	if sequence.Status == SequenceSuspended {
		return Sequence{}, errors.New("sludge sequence is suspended for maintenance")
	}
	if _, err := b.pumps.Ready(sequence.PumpID, sequence.ID, sequence.Generation, flow, now); err != nil {
		return Sequence{}, err
	}
	sequence.Status = SequenceRunning
	sequence.UpdatedAt = now.UTC()
	b.byLine[lineID] = sequence
	return sequence, nil
}

func (b *SequenceBook) Fail(lineID model.LineID, now time.Time) (Sequence, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sequence, ok := b.byLine[lineID]
	if !ok {
		return Sequence{}, errors.New("sludge sequence is not active")
	}
	sequence.Status = SequenceFailed
	sequence.UpdatedAt = now.UTC()
	b.byLine[lineID] = sequence
	compensation, err := journal.NewCompensation(lineID, sequence.ID, sequence.Generation, sequence.PumpID, "stop", now.Add(time.Second))
	if err != nil {
		return Sequence{}, err
	}
	b.queue.Schedule(compensation)
	return sequence, nil
}

// Suspend parks an in-flight sequence while its pump is locked out for
// maintenance. Ownership and the pump assignment are preserved so the action
// can be resumed instead of being torn down.
func (b *SequenceBook) Suspend(lineID model.LineID, now time.Time) (Sequence, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sequence, ok := b.byLine[lineID]
	if !ok {
		return Sequence{}, errors.New("sludge sequence is not active")
	}
	if sequence.Status == SequenceFailed || sequence.Status == SequenceSuspended {
		return sequence, nil
	}
	sequence.Status = SequenceSuspended
	sequence.UpdatedAt = now.UTC()
	b.byLine[lineID] = sequence
	return sequence, nil
}

// Resume returns a suspended sequence to the switching phase after its pump is
// released; field must re-confirm flow (pre-run check) before it runs again.
func (b *SequenceBook) Resume(lineID model.LineID, now time.Time) (Sequence, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sequence, ok := b.byLine[lineID]
	if !ok {
		return Sequence{}, errors.New("sludge sequence is not active")
	}
	if sequence.Status != SequenceSuspended {
		return sequence, nil
	}
	sequence.Status = SequenceSwitching
	sequence.UpdatedAt = now.UTC()
	b.byLine[lineID] = sequence
	return sequence, nil
}

// PumpSequence returns the sequence currently owning a pump, if any.
func (b *SequenceBook) PumpSequence(pumpID string) (Sequence, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sequence := range b.byLine {
		if sequence.PumpID == pumpID {
			return sequence, true
		}
	}
	return Sequence{}, false
}

func (b *SequenceBook) ExecuteCompensation(ctx context.Context, compensation journal.Compensation, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if compensation.Action != "stop" {
		return errors.New("unsupported sludge compensation action")
	}
	_, err := b.pumps.StopOwned(compensation.DeviceID, compensation.SequenceID, compensation.Generation, now)
	return err
}

func (b *SequenceBook) Current(lineID model.LineID) (Sequence, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	sequence, ok := b.byLine[lineID]
	return sequence, ok
}
