// Package store persists download operation intents before callers change an
// upstream service. It never executes or retries an operation itself.
package store

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrConflict = errors.New("idempotency key was already used with a different request")
	ErrNotFound = errors.New("operation not found")
	ErrLocked   = errors.New("operation store is already open in another instance")
	ErrClosed   = errors.New("operation store is closed")
)

// Operation is a durable intent and its latest observed result. Details can
// contain private request parameters and must not be returned by public APIs.
type Operation struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	TaskID         string          `json:"task_id,omitempty"`
	PreviousTaskID string          `json:"previous_task_id,omitempty"`
	Status         string          `json:"status"`
	Message        string          `json:"message,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	Result         json.RawMessage `json:"result,omitempty"`
	Details        json.RawMessage `json:"details,omitempty"`
}
