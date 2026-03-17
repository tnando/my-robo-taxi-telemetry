package events

import "context"

// Bus is the central event dispatcher. All components publish and subscribe
// through it. The bus provides topic-based fan-out: a published event is
// delivered to every subscriber registered for that event's topic.
//
// Implementations must be safe for concurrent use by multiple goroutines.
type Bus interface {
	// Publish sends an event to all subscribers of the event's topic.
	// It never blocks on slow subscribers. Returns ErrBusClosed if the
	// bus has been shut down.
	Publish(ctx context.Context, event Event) error

	// Subscribe registers a handler for a topic and returns a Subscription
	// that can be used to unsubscribe later. Returns ErrBusClosed if the
	// bus has been shut down.
	Subscribe(topic Topic, handler Handler) (Subscription, error)

	// Unsubscribe removes a subscription by its ID. After Unsubscribe
	// returns, the handler will not be called again. Returns
	// ErrSubscriptionNotFound if the subscription does not exist.
	Unsubscribe(sub Subscription) error

	// Close gracefully shuts down the bus. It stops accepting new publishes,
	// drains pending events up to the context deadline, and tears down all
	// subscriber goroutines.
	Close(ctx context.Context) error
}
