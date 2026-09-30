package maintenance

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Store persists active lockouts independently of the line journal so a
// lockout keeps blocking control commands after a restart even when its
// journal event precedes the latest line snapshot.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore opens (or creates on first save) the lockout state file.
func NewStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("maintenance store directory is required")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dataDir, "lockouts.json")}, nil
}

// Save atomically writes the active lockouts.
func (s *Store) Save(records []Lockout) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.Slice(records, func(i, j int) bool { return records[i].Device.ID < records[j].Device.ID })
	data, err := json.MarshalIndent(struct {
		Lockouts []Lockout `json:"lockouts"`
	}{Lockouts: records}, "", "  ")
	if err != nil {
		return err
	}
	temporary := s.path + ".next"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o640); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}

// Load reads the persisted lockouts. A missing file yields no records.
func (s *Store) Load() ([]Lockout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var payload struct {
		Lockouts []Lockout `json:"lockouts"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	return payload.Lockouts, nil
}
