package events

// EventPayload is implemented by every domain event struct.
// The unexported marker method seals the interface to this package,
// ensuring that only payload types defined here can be used.
type EventPayload interface {
	eventPayload() // sealed marker — unexported to prevent external implementations
	EventTopic() Topic
}

// BasePayload is embedded by concrete payload types to satisfy the sealed
// EventPayload marker method. Domain event structs embed this instead of
// implementing eventPayload() themselves.
type BasePayload struct{}

func (BasePayload) eventPayload() {}
