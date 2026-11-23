package router

import (
	"encoding/json"
	"errors"
)

var (
	// ErrInvalidEvent indicates an unknown event, unsupported version, or
	// invalid event payload.
	ErrInvalidEvent = errors.New("invalid event")
	// ErrUIDConflict indicates that an event UID was reused for different data.
	ErrUIDConflict = errors.New("event UID conflict")
)

// Event is a persisted event instance.
type Event struct {
	Seq         int64           `json:"seq"`
	UID         string          `json:"uid"`
	ID          string          `json:"id"`
	Version     uint32          `json:"version"`
	Payload     json.RawMessage `json:"payload"`
	Source      string          `json:"source"`
	EmittedAt   int64           `json:"emitted_at"`
	Correlation *string         `json:"correlation"`
}

// EmitRequest describes an event to emit. An empty UID requests a generated UID.
type EmitRequest struct {
	UID         string          `json:"uid"`
	ID          string          `json:"id"`
	Version     uint32          `json:"version"`
	Payload     json.RawMessage `json:"payload"`
	Source      string          `json:"source"`
	Correlation *string         `json:"correlation"`
}

// EmitResult reports persistence and idempotent replay outcomes.
type EmitResult struct {
	Event    Event `json:"event"`
	Stored   bool  `json:"stored"`
	Replayed bool  `json:"replayed"`
}

// Delivery is the persisted state of one plugin's event delivery.
type Delivery struct {
	EventSeq      int64   `json:"event_seq"`
	Plugin        string  `json:"plugin"`
	State         string  `json:"state"`
	Attempts      int64   `json:"attempts"`
	NextAttemptAt int64   `json:"next_attempt_at"`
	LastError     *string `json:"last_error"`
	UpdatedAt     int64   `json:"updated_at"`
}
