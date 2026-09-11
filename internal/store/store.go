package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const stateFileName = "operations.json"

type keyEntry struct {
	Hash        string `json:"request_hash"`
	OperationID string `json:"operation_id"`
}

type diskState struct {
	Version    int                  `json:"version"`
	Operations map[string]Operation `json:"operations"`
	Keys       map[string]keyEntry  `json:"idempotency"`
}

// Store is safe for concurrent goroutines. The advisory lock protects against
// other instances that use this package; all access must go through one Store.
type Store struct {
	mu       sync.Mutex
	dir      string
	lock     *os.File
	state    diskState
	closed   bool
	writeErr error
}

// Open creates or loads a store. Invalid existing data is never overwritten.
// The directory should be on a local filesystem that supports flock and fsync.
func Open(dir string) (_ *Store, err error) {
	if dir == "" {
		return nil, errors.New("operation store directory is required")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve operation store directory: %w", err)
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create operation store directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect operation store directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("operation store directory must be a real directory, not a symlink")
	}
	if err = os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure operation store directory: %w", err)
	}
	lockPath := filepath.Join(dir, ".lock")
	if err = requireRegularFile(lockPath); err != nil {
		return nil, fmt.Errorf("inspect operation store lock: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open operation store lock: %w", err)
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	if err = lock.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure operation store lock: %w", err)
	}
	if err = lockExclusive(lock); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lock: lock, state: diskState{
		Version: 1, Operations: make(map[string]Operation), Keys: make(map[string]keyEntry),
	}}
	statePath := filepath.Join(dir, stateFileName)
	if err = requireRegularFile(statePath); err != nil {
		return nil, fmt.Errorf("inspect operation store data: %w", err)
	}
	f, err := os.Open(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open operation store data: %w", err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	var loaded diskState
	if err = decoder.Decode(&loaded); err != nil {
		return nil, fmt.Errorf("invalid operation store data: %w", err)
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid operation store data: unexpected trailing data")
	}
	if err = validateState(loaded); err != nil {
		return nil, fmt.Errorf("invalid operation store data: %w", err)
	}
	if err = os.Chmod(statePath, 0o600); err != nil {
		return nil, fmt.Errorf("secure operation store data: %w", err)
	}
	s.state = loaded
	return s, nil
}

// Close releases the process lock. It is safe to call more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Close()
}

// Begin durably records an operation before its caller makes any upstream
// change. A matching key and hash returns the original operation with replay
// true, including after a process crash. Empty keys bypass deduplication.
func (s *Store) Begin(key, hash string, op Operation) (Operation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return Operation{}, false, err
	}
	if key != "" {
		if existing, ok := s.state.Keys[key]; ok {
			if existing.Hash != hash {
				return Operation{}, false, ErrConflict
			}
			return cloneOperation(s.state.Operations[existing.OperationID]), true, nil
		}
	}
	if op.ID == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Operation{}, false, fmt.Errorf("generate operation id: %w", err)
		}
		op.ID = "op_" + hex.EncodeToString(b[:])
	}
	if _, ok := s.state.Operations[op.ID]; ok {
		return Operation{}, false, fmt.Errorf("operation id already exists: %w", ErrConflict)
	}
	now := time.Now().UTC()
	if op.CreatedAt.IsZero() {
		op.CreatedAt = now
	}
	op.UpdatedAt = now
	if op.Status == "" {
		op.Status = "planned"
	}
	if err := validateOperation(op); err != nil {
		return Operation{}, false, err
	}
	newState := cloneState(s.state)
	newState.Operations[op.ID] = cloneOperation(op)
	if key != "" {
		newState.Keys[key] = keyEntry{Hash: hash, OperationID: op.ID}
	}
	if err := s.commit(newState); err != nil {
		return Operation{}, false, err
	}
	return cloneOperation(op), false, nil
}

func (s *Store) Find(id string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return Operation{}, err
	}
	op, ok := s.state.Operations[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	return cloneOperation(op), nil
}

// Update atomically replaces the current result of an existing operation.
// ID, Kind and CreatedAt identify the original intent and remain unchanged.
func (s *Store) Update(op Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	existing, ok := s.state.Operations[op.ID]
	if !ok {
		return ErrNotFound
	}
	if op.Kind != existing.Kind {
		return errors.New("operation kind cannot change")
	}
	op.CreatedAt = existing.CreatedAt
	op.UpdatedAt = time.Now().UTC()
	if err := validateOperation(op); err != nil {
		return err
	}
	newState := cloneState(s.state)
	newState.Operations[op.ID] = cloneOperation(op)
	return s.commit(newState)
}

// Pending returns intents that require observation or reconciliation. It never
// authorizes resubmission, including for records still marked planned after a
// restart. The result is ordered by creation time, then ID.
func (s *Store) Pending() []Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Operation, 0)
	if s.closed || s.writeErr != nil {
		return result
	}
	for _, op := range s.state.Operations {
		if op.Status == "planned" || op.Status == "accepted" || op.Status == "unknown_outcome" {
			result = append(result, cloneOperation(op))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result
}

func (s *Store) writable() error {
	if s.closed {
		return ErrClosed
	}
	if s.writeErr != nil {
		return fmt.Errorf("operation store requires reopening after a persistence error: %w", s.writeErr)
	}
	return nil
}

func (s *Store) commit(state diskState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode operation store data: %w", err)
	}
	if err := atomicWrite(s.dir, data); err != nil {
		// Rename may already have succeeded. Reject further writes until the
		// store is reopened, so no caller can act on uncertain persistence.
		s.writeErr = err
		return fmt.Errorf("persist operation store data: %w", err)
	}
	s.state = state
	return nil
}

func atomicWrite(dir string, data []byte) (err error) {
	f, err := os.CreateTemp(dir, ".operations-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, stateFileName)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("must be a regular file, not a directory or symlink")
	}
	return nil
}

func validateState(state diskState) error {
	if state.Version != 1 {
		return errors.New("unsupported store version")
	}
	if state.Operations == nil || state.Keys == nil {
		return errors.New("operation and idempotency indexes are required")
	}
	for id, op := range state.Operations {
		if id != op.ID {
			return errors.New("operation index does not match operation id")
		}
		if err := validateOperation(op); err != nil {
			return err
		}
	}
	for key, entry := range state.Keys {
		if key == "" {
			return errors.New("empty idempotency key in index")
		}
		if _, ok := state.Operations[entry.OperationID]; !ok {
			return errors.New("idempotency index references a missing operation")
		}
	}
	return nil
}

func validateOperation(op Operation) error {
	if op.ID == "" || op.Kind == "" {
		return errors.New("operation id and kind are required")
	}
	if op.CreatedAt.IsZero() || op.UpdatedAt.IsZero() {
		return errors.New("operation timestamps are required")
	}
	switch op.Status {
	case "planned", "accepted", "confirmed", "failed", "unknown_outcome":
	default:
		return errors.New("invalid operation status")
	}
	if (len(op.Result) > 0 && !json.Valid(op.Result)) || (len(op.Details) > 0 && !json.Valid(op.Details)) {
		return errors.New("operation result and details must be valid JSON")
	}
	return nil
}

func cloneOperation(op Operation) Operation {
	op.Result = append(json.RawMessage(nil), op.Result...)
	op.Details = append(json.RawMessage(nil), op.Details...)
	return op
}

func cloneState(original diskState) diskState {
	cloned := diskState{Version: original.Version,
		Operations: make(map[string]Operation, len(original.Operations)),
		Keys:       make(map[string]keyEntry, len(original.Keys)),
	}
	// Existing operations are immutable; writes replace whole records and
	// every input/output RawMessage is copied at the public API boundary.
	for id, op := range original.Operations {
		cloned.Operations[id] = op
	}
	for key, entry := range original.Keys {
		cloned.Keys[key] = entry
	}
	return cloned
}
