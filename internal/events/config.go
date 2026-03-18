package events

import "time"

// BusConfig holds tunable parameters for the channel-based bus implementation.
type BusConfig struct {
	// BufferSize is the capacity of each subscriber's buffered channel.
	// When the buffer is full, the oldest event is dropped to make room.
	// Default: 256.
	BufferSize int

	// DrainTimeout is the maximum time Close will wait for pending events
	// to be delivered before forcibly shutting down subscriber goroutines.
	// Default: 5s.
	DrainTimeout time.Duration
}

// DefaultBusConfig returns a BusConfig with sensible production defaults.
func DefaultBusConfig() BusConfig {
	return BusConfig{
		BufferSize:   256,
		DrainTimeout: 5 * time.Second,
	}
}
