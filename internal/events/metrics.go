package events

// BusMetrics collects event bus operational metrics. Implementations are
// expected to be safe for concurrent use by multiple goroutines.
type BusMetrics interface {
	// IncPublished increments the count of events published to a topic.
	IncPublished(topic Topic)

	// IncDelivered increments the count of events successfully delivered
	// to a subscriber for a topic.
	IncDelivered(topic Topic)

	// IncDropped increments the count of events dropped due to a slow
	// subscriber whose buffer was full.
	IncDropped(topic Topic)

	// ObservePublishDuration records the time taken to fan out a single
	// Publish call across all subscribers of a topic.
	ObservePublishDuration(topic Topic, seconds float64)

	// SetSubscriberCount sets the current number of active subscribers
	// for a topic.
	SetSubscriberCount(topic Topic, count int)
}
