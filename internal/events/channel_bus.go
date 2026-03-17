package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ChannelBus is a channel-based Bus implementation with per-subscriber
// buffered channels, fan-out delivery, and drop-oldest backpressure.
//
// Each subscriber gets its own goroutine and buffered channel. The Publish
// method never blocks: if a subscriber's buffer is full, the oldest event
// is evicted to make room for the new one.
type ChannelBus struct {
	cfg     BusConfig
	metrics BusMetrics
	logger  *slog.Logger

	mu     sync.RWMutex
	topics map[Topic]*topicEntry
	closed atomic.Bool
	wg     sync.WaitGroup // tracks subscriber goroutines
}

type topicEntry struct {
	mu          sync.RWMutex
	subscribers map[string]*subscriber
}

type subscriber struct {
	id      string
	ch      chan Event
	handler Handler
	done    chan struct{} // closed to signal the goroutine to stop
}

var _ Bus = (*ChannelBus)(nil)

// NewChannelBus creates a ChannelBus. Zero-value fields in cfg are replaced
// with DefaultBusConfig defaults.
func NewChannelBus(cfg BusConfig, metrics BusMetrics, logger *slog.Logger) *ChannelBus {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = DefaultBusConfig().BufferSize
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultBusConfig().DrainTimeout
	}
	return &ChannelBus{
		cfg:     cfg,
		metrics: metrics,
		logger:  logger,
		topics:  make(map[Topic]*topicEntry),
	}
}

// Publish sends an event to all subscribers of its topic. It never blocks
// on slow subscribers. Returns ErrBusClosed after shutdown.
func (b *ChannelBus) Publish(ctx context.Context, event Event) error {
	if b.closed.Load() {
		return ErrBusClosed
	}

	start := time.Now()
	topic := event.Topic
	b.metrics.IncPublished(topic)

	te := b.getTopicEntry(topic)
	if te == nil {
		b.metrics.ObservePublishDuration(topic, time.Since(start).Seconds())
		return nil
	}

	te.mu.RLock()
	subs := make([]*subscriber, 0, len(te.subscribers))
	for _, s := range te.subscribers {
		subs = append(subs, s)
	}
	te.mu.RUnlock()

	for _, s := range subs {
		b.sendToSubscriber(s, event)
	}

	b.metrics.ObservePublishDuration(topic, time.Since(start).Seconds())
	return nil
}

// sendToSubscriber performs a non-blocking send. If the buffer is full, it
// evicts the oldest event (drop-oldest backpressure) before retrying.
func (b *ChannelBus) sendToSubscriber(s *subscriber, event Event) {
	select {
	case s.ch <- event:
		return
	default:
	}
	// Buffer full — drop oldest.
	select {
	case <-s.ch:
		b.metrics.IncDropped(event.Topic)
		b.logger.Warn("dropped oldest event for slow subscriber",
			slog.String("subscriber_id", s.id),
			slog.String("topic", string(event.Topic)),
			slog.String("event_id", event.ID),
		)
	default:
		// drained concurrently
	}
	select {
	case s.ch <- event:
	default:
		b.metrics.IncDropped(event.Topic)
	}
}

// Subscribe registers a handler for a topic. The handler runs in a
// dedicated goroutine. Returns ErrBusClosed after shutdown.
func (b *ChannelBus) Subscribe(topic Topic, handler Handler) (Subscription, error) {
	if b.closed.Load() {
		return Subscription{}, ErrBusClosed
	}

	sub := &subscriber{
		id:      generateID(),
		ch:      make(chan Event, b.cfg.BufferSize),
		handler: handler,
		done:    make(chan struct{}),
	}

	te := b.getOrCreateTopicEntry(topic)
	te.mu.Lock()
	te.subscribers[sub.id] = sub
	count := len(te.subscribers)
	te.mu.Unlock()

	b.metrics.SetSubscriberCount(topic, count)
	b.logger.Debug("subscriber registered",
		slog.String("subscriber_id", sub.id),
		slog.String("topic", string(topic)),
		slog.Int("subscriber_count", count),
	)

	b.wg.Add(1)
	go b.deliverLoop(sub, topic)

	return Subscription{ID: sub.id, Topic: topic}, nil
}

// deliverLoop reads events from the subscriber's channel and calls the
// handler until the done channel is closed, then drains remaining events.
func (b *ChannelBus) deliverLoop(s *subscriber, topic Topic) {
	defer b.wg.Done()
	for {
		select {
		case <-s.done:
			b.drainSubscriber(s, topic)
			return
		case event, ok := <-s.ch:
			if !ok {
				return
			}
			s.handler(event)
			b.metrics.IncDelivered(topic)
		}
	}
}

// drainSubscriber delivers remaining buffered events after the stop signal.
func (b *ChannelBus) drainSubscriber(s *subscriber, topic Topic) {
	for {
		select {
		case event, ok := <-s.ch:
			if !ok {
				return
			}
			s.handler(event)
			b.metrics.IncDelivered(topic)
		default:
			return
		}
	}
}

// Unsubscribe removes a subscription and stops its delivery goroutine.
func (b *ChannelBus) Unsubscribe(sub Subscription) error {
	te := b.getTopicEntry(sub.Topic)
	if te == nil {
		return fmt.Errorf("unsubscribe(%s): %w", sub.ID, ErrSubscriptionNotFound)
	}

	te.mu.Lock()
	s, ok := te.subscribers[sub.ID]
	if !ok {
		te.mu.Unlock()
		return fmt.Errorf("unsubscribe(%s): %w", sub.ID, ErrSubscriptionNotFound)
	}
	delete(te.subscribers, sub.ID)
	count := len(te.subscribers)
	te.mu.Unlock()

	b.metrics.SetSubscriberCount(sub.Topic, count)
	close(s.done)

	b.logger.Debug("subscriber unsubscribed",
		slog.String("subscriber_id", sub.ID),
		slog.String("topic", string(sub.Topic)),
		slog.Int("subscriber_count", count),
	)
	return nil
}

// Close gracefully shuts down the bus: stops accepting publishes, signals
// all subscriber goroutines to drain, and waits up to the context deadline
// or DrainTimeout.
func (b *ChannelBus) Close(ctx context.Context) error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	b.logger.Info("event bus shutting down, draining pending events")

	b.mu.RLock()
	var allSubs []*subscriber
	for _, te := range b.topics {
		te.mu.RLock()
		for _, s := range te.subscribers {
			allSubs = append(allSubs, s)
		}
		te.mu.RUnlock()
	}
	b.mu.RUnlock()

	for _, s := range allSubs {
		close(s.done)
	}

	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()

	drainCtx, cancel := context.WithTimeout(ctx, b.cfg.DrainTimeout)
	defer cancel()

	select {
	case <-done:
		b.logger.Info("event bus drained and stopped cleanly")
	case <-drainCtx.Done():
		b.logger.Warn("event bus drain timed out, some events may have been lost")
	}
	return nil
}

func (b *ChannelBus) getTopicEntry(topic Topic) *topicEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.topics[topic]
}

func (b *ChannelBus) getOrCreateTopicEntry(topic Topic) *topicEntry {
	b.mu.RLock()
	te, ok := b.topics[topic]
	b.mu.RUnlock()
	if ok {
		return te
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if te, ok = b.topics[topic]; ok {
		return te
	}
	te = &topicEntry{subscribers: make(map[string]*subscriber)}
	b.topics[topic] = te
	return te
}
