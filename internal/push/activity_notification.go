package push

import (
	"context"
	"time"
)

// ONE ACTIVITY PUSH, as the sender sees it — who it addresses, which of the
// three events it is, and the three headers that are computed from it.
//
// Split from activity.go so both stay inside the 300-line cap, along the seam
// the two already had: that file is the CONTENT STATE, which is a contract with
// an installed Swift struct and changes for product reasons, while this is the
// ENVELOPE, which changes for APNs reasons. They have never moved together and
// there is no reason they should start.

// ActivityNotification is one addressed Live Activity update.
type ActivityNotification struct {
	// ActivityToken is the ActivityKit update token. P1 — never log in full.
	ActivityToken string
	// Sandbox selects the APNs sandbox host (development builds).
	Sandbox bool
	// Event is `update` or `end`.
	Event ActivityEvent
	// ContentState is the state the Activity will display.
	ContentState ActivityContentState
	// Timestamp is when this state was true. ActivityKit uses it to discard an
	// update that arrives out of order, which the network makes routine.
	Timestamp time.Time
	// DismissalDate, set only on an end event, is when iOS removes the Activity
	// from the lock screen. Nil on an end event means "dismiss immediately".
	DismissalDate *time.Time
	// LowPriority sends at apns-priority 5 instead of 10. ⚠️ NO PRODUCTION
	// CALLER SETS IT SINCE MYR-573: the ETA ticker rode it per MYR-194
	// decision 3, and field evidence reversed that — priority-5 Activity
	// updates are deferred indefinitely on a locked phone, so the card only
	// ever moved on lifecycle alerts. Kept as the deliberate retreat shape
	// (one line in the ticker re-enables it if Apple ever throttles the
	// immediate budget), and pinned by TestActivityTickUsesConservingPriority
	// so the header mapping cannot rot. Ignored when Alert is set — see
	// priority().
	LowPriority bool
	// Start, set only on an ActivityEventStart, carries the values that go into
	// `aps.attributes` (MYR-602). Nil on every update and every end, and nil on
	// a start is a programming error the payload builder absorbs by writing no
	// attributes — which iOS answers by ignoring the push, so the caller's own
	// tests are the guard.
	Start *TripActivityStart
	// Alert, when set, adds an `aps.alert` dictionary that makes iOS expand the
	// Dynamic Island for ~3s (MYR-398). Nil on all but the six phase changes;
	// see activity_alert.go for why "nil" is the overwhelmingly common case.
	Alert *ActivityAlert
}

// ActivitySender delivers ActivityKit remote updates.
//
// Defined at the consumer site, like Sender beside it, so the Live Activity
// notifier can be tested against a spy without an APNs key.
type ActivitySender interface {
	SendActivity(ctx context.Context, n ActivityNotification) error
}

// StaleDate is when this update stops being trustworthy.
func (n ActivityNotification) StaleDate() time.Time {
	return n.Timestamp.Add(StaleAfter)
}

// priority renders the apns-priority header for this update.
//
// AN ALERTING UPDATE IS ALWAYS IMMEDIATE, whatever the caller asked for — a
// push whose entire purpose is to open the island for three seconds is not a
// refresh that can afford to be dropped or coalesced. (Historical note: until
// MYR-573 the ETA ticker set LowPriority, so this promotion was load-bearing on
// every Arriving alert; today no production caller sets the flag and the guard
// is defence for the retreat path.)
func (n ActivityNotification) priority() string {
	if n.LowPriority && n.Alert == nil {
		return priorityConserving
	}
	return priorityImmediate
}
