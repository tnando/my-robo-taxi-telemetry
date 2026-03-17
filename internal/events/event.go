package events

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Event is the envelope carried through the bus. Events are immutable after
// creation — subscribers must never modify a received event.
type Event struct {
	// ID uniquely identifies this event instance. Generated at creation time.
	ID string

	// Topic identifies the event channel this event belongs to.
	Topic Topic

	// Timestamp records when the event was created.
	Timestamp time.Time

	// Payload is the typed domain event data. Use a type assertion at the
	// subscriber site to extract the concrete type.
	Payload EventPayload
}

// NewEvent creates an Event with a generated ID and the current timestamp.
// The topic is derived from the payload's EventTopic method.
func NewEvent(payload EventPayload) Event {
	return Event{
		ID:        generateID(),
		Topic:     payload.EventTopic(),
		Timestamp: time.Now(),
		Payload:   payload,
	}
}

// generateID produces a random hex-encoded identifier suitable for event IDs.
// It uses crypto/rand for uniqueness without requiring external dependencies.
func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
