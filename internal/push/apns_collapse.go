package push

import (
	"crypto/sha256"
	"encoding/hex"
)

// The `apns-collapse-id` header (MYR-554).
//
// THE BUG IT FIXES IS A DOUBLE-DELIVERY THE SERVER CANNOT SEE. deliver() retries
// one attempt on a network error or a 5xx, and "network error" includes every
// failure that happens AFTER the request left this process: a connection reset
// while the response was being read, a timeout that fired between Apple
// accepting the POST and the ACK arriving, an h2 stream torn down mid-frame.
// In all of those Apple has the notification and the retry sends it AGAIN. Prod
// 2026-08-13 is the proof: two identical owner "time to head out" banners on one
// device, from one device row, behind a claim-guarded publish that ran once —
// so neither the event, nor the fan-out, nor the registry duplicated anything.
// Only the transport did.
//
// THE FIX IS APPLE'S OWN DE-DUPLICATION. Per the APNs provider API (Sending
// notification requests to APNs → "apns-collapse-id"), two notifications
// carrying the SAME collapse id are shown to the user as ONE — the later
// replaces the earlier — and the header's value "must not exceed 64 bytes".
// Giving the two attempts of one send the same id therefore makes a redelivery
// idempotent AT APPLE, which is the only place that can see both copies.
//
// THE ID IS THE NOTIFICATION'S INTENT, not its attempt: `{rideID}:{eventTopic}`.
// It is computed once in Send and carried on the message, so both trips through
// the retry loop present the same value, and it is derived from nothing
// per-attempt (no timestamp, no counter) — an id that varied per attempt would
// be a header Apple honours and a bug it cannot help with.
//
// WHAT ELSE COLLAPSES, stated plainly because it is a behaviour change and not
// only a bug fix. Two alerts for the SAME ride on the SAME topic now merge in
// Notification Center: an `accepted` banner followed later by an `arrived`
// banner (both `ride.status.changed`) leaves the newer one in the list. That is
// the intended reading of a ride — the freshest state of one journey is the one
// worth keeping, and a stack of superseded banners for a ride already under way
// is the noise the Live Activity exists to replace. DELIVERY AND ALERTING ARE
// UNAFFECTED: collapsing merges the entry, it does not suppress the push, so
// every banner still buzzes the phone when it lands.
//
// NOT ON ACTIVITYKIT PUSHES. An Activity update already addresses exactly one
// card through its own update token, so it has nothing to collapse against.
// buildActivityMessage therefore leaves the field empty and the header is
// omitted — see newRequest.

// maxCollapseIDBytes is Apple's documented cap on the `apns-collapse-id` header
// value. A longer value is rejected (400 BadCollapseId), so the id is truncated
// rather than sent whole.
const maxCollapseIDBytes = 64

// collapseID renders the de-duplication key for one alert, or "" when there is
// nothing safe to key on.
//
// AN EMPTY RIDE ID YIELDS NO HEADER, and that is a correctness rule rather than
// tidiness. Every alert this package sends today names a ride, but a future
// fan-out that does not would otherwise collapse on the topic ALONE — merging
// unrelated notifications for unrelated subjects into one line on somebody's
// lock screen. Absent is the only safe answer, and it restores exactly the
// pre-MYR-554 behaviour for such a push: at-least-once, never over-merged.
//
// AN OVERSIZED ID IS HASHED, NOT TRUNCATED, and MYR-602 is why that is not a
// refinement but a fix.
//
// Truncation was chosen when every subject was one cuid (~25 bytes) plus a
// dotted topic — comfortably inside the cap, with the cut as a guard that could
// not fire. A TRIP LEG's subject is TWO ids, `{tripID}|{legID}`, because two
// consecutive legs of one trip must not merge their banners; with real ids that
// is ~67 bytes before the topic is appended, so the cut fires on every leg push
// and it removes THE DISCRIMINATING TAIL. `trip_leg_started` and
// `trip_leg_arrived` on the same leg share their whole prefix and differ only
// in the topic at the end, so both truncated to the SAME value and Apple merged
// them: a participant who missed the departure banner found only the arrival,
// and one who read the departure had it silently replaced.
//
// The old comment argued that over-collapsing was the safe direction to fail
// in, against the alternative of a 400 that loses the push. It was right about
// the alternative and wrong that those are the only two: a digest is under the
// cap AND preserves the distinction. The prefix marks the value as a digest for
// anyone reading a packet capture, and 128 bits of SHA-256 is far past any
// accidental collision across a fleet's notifications.
//
// Ids that FIT are still sent verbatim, deliberately: they are the vast
// majority, and a readable `crr_…:ride.status.changed` in a capture is worth
// keeping.
func collapseID(subject, eventTopic string) string {
	if subject == "" {
		return ""
	}
	id := subject + ":" + eventTopic
	if len(id) <= maxCollapseIDBytes {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return collapseDigestPrefix + hex.EncodeToString(sum[:16])
}

// collapseDigestPrefix marks a hashed collapse id in logs and packet captures,
// so a value that does not look like its subject is explicable rather than
// mysterious. Two characters, leaving the digest itself the bulk of a value
// that is 34 bytes against a 64-byte cap.
const collapseDigestPrefix = "h."
