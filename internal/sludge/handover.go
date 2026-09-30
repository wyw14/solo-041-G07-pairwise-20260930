package sludge

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/maintenance"
	"github.com/wyw14/cry-97/internal/model"
)

type HandoverPhase string

const (
	HandoverPriming   HandoverPhase = "priming"
	HandoverCompleted HandoverPhase = "completed"
	HandoverSuspended HandoverPhase = "suspended"
)

type Handover struct {
	ID          uuid.UUID     `json:"id"`
	LineID      model.LineID  `json:"line_id"`
	FromPump    string        `json:"from_pump"`
	ToPump      string        `json:"to_pump"`
	Generation  uint64        `json:"generation"`
	Phase       HandoverPhase `json:"phase"`
	StartedAt   time.Time     `json:"started_at"`
	CompletedAt *time.Time    `json:"completed_at,omitempty"`
}

type HandoverBook struct {
	mu      sync.RWMutex
	current map[model.LineID]Handover
}

func NewHandoverBook() *HandoverBook {
	return &HandoverBook{current: make(map[model.LineID]Handover)}
}

func (b *HandoverBook) Start(lineID model.LineID, fromPump, toPump string, generation uint64, now time.Time) (Handover, error) {
	if lineID == "" || fromPump == "" || toPump == "" || fromPump == toPump || generation == 0 {
		return Handover{}, errors.New("pump handover is invalid")
	}
	handover := Handover{
		ID: uuid.New(), LineID: lineID, FromPump: fromPump, ToPump: toPump,
		Generation: generation, Phase: HandoverPriming, StartedAt: now.UTC(),
	}
	b.mu.Lock()
	b.current[lineID] = handover
	b.mu.Unlock()
	return handover, nil
}

func (b *HandoverBook) ConfirmReady(lineID model.LineID, pumpID string, generation uint64, flow float64, now time.Time) (Handover, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	handover, ok := b.current[lineID]
	if !ok {
		return Handover{}, errors.New("pump handover is not active")
	}
	if handover.Phase == HandoverSuspended {
		return Handover{}, maintenance.ErrDeviceLockedOut
	}
	if handover.Generation != generation || handover.ToPump != pumpID {
		return Handover{}, errors.New("pump ready confirmation does not belong to current handover")
	}
	if flow <= 0 {
		return Handover{}, errors.New("new duty pump has no confirmed flow")
	}
	completed := now.UTC()
	handover.Phase = HandoverCompleted
	handover.CompletedAt = &completed
	b.current[lineID] = handover
	return handover, nil
}

func (b *HandoverBook) Current(lineID model.LineID) (Handover, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	handover, ok := b.current[lineID]
	return handover, ok
}

// Suspend freezes an in-flight pump handover touching the locked-out pump.
func (b *HandoverBook) Suspend(pumpID string, now time.Time) (Handover, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for lineID, handover := range b.current {
		if handover.Phase != HandoverPriming {
			continue
		}
		if handover.FromPump != pumpID && handover.ToPump != pumpID {
			continue
		}
		handover.Phase = HandoverSuspended
		b.current[lineID] = handover
		return handover, true
	}
	return Handover{}, false
}

// Resume returns a suspended handover touching the given pump to the
// priming gate so the duty pump flow must be re-confirmed after maintenance.
func (b *HandoverBook) Resume(lineID model.LineID, pumpID string, now time.Time) (Handover, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	handover, ok := b.current[lineID]
	if !ok || handover.Phase != HandoverSuspended {
		return Handover{}, false
	}
	if handover.FromPump != pumpID && handover.ToPump != pumpID {
		return Handover{}, false
	}
	handover.Phase = HandoverPriming
	b.current[lineID] = handover
	return handover, true
}
