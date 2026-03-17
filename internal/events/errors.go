package events

import "errors"

var (
	// ErrBusClosed is returned when Publish or Subscribe is called on a
	// bus that has been shut down via Close.
	ErrBusClosed = errors.New("event bus is closed")

	// ErrSubscriptionNotFound is returned by Unsubscribe when the given
	// subscription ID does not match any active subscription.
	ErrSubscriptionNotFound = errors.New("subscription not found")
)
