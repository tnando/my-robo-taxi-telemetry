package events

// Handler is the callback signature for event subscribers. Handlers are
// invoked in a dedicated goroutine per subscriber, so they do not need to
// be safe for concurrent use — the bus guarantees serial delivery per
// subscription.
type Handler func(Event)

// Subscription is returned by Bus.Subscribe and holds the information
// needed to unsubscribe later.
type Subscription struct {
	// ID uniquely identifies this subscription within the bus.
	ID string

	// Topic is the topic this subscription is registered for.
	Topic Topic
}
