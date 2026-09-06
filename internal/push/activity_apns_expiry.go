package push

import "time"

// THE `apns-expiration` DECISION: how long Apple should hold each shape of
// Activity push for a phone that is off or out of signal.
//
// Split from activity_apns.go so both stay inside the 300-line cap, and along a
// seam worth having on its own page: this is FOUR different horizons for four
// shapes, each argued from what happens when a phone reconnects late, and the
// argument is the whole content — the code is one function with three branches.
// Getting one of them wrong is invisible in every test and every log: APNs
// simply drops a push nobody was waiting for.

// endPushRetention is how long APNs holds an undelivered `end` push for a phone
// that is off or out of signal. A day is far longer than any ride and far
// shorter than the ActivityKit ceiling; it exists to outlast a flat battery or
// an overnight flight, not to be precise.
const endPushRetention = 24 * time.Hour

// pushToStartRetention is how long APNs holds an undelivered PUSH-TO-START.
//
// FIFTEEN MINUTES, and the number is a compromise between the two ways this
// push can be wrong. Pinned to the 3-minute stale-date like an ordinary update,
// a phone in a tunnel or in a pocket at the moment a leg begins would never get
// the card at all — and unlike an ETA tick there is no successor, because the
// updater only pushes to Activities that already exist. Given the `end`'s day,
// a card would materialise on a lock screen for a leg that finished hours ago,
// which is worse than never appearing: an Activity created after its journey
// ended has no update coming and no end coming, and sits there until
// ActivityKit's own ceiling.
//
// Fifteen minutes is long enough to outlast a tunnel, a lift, or a locked phone
// in a bag, and short enough that the card is still about a leg the car is
// plausibly still driving — the median leg on a road trip is far longer. A
// start that misses this window costs the card, not the trip: the
// `trip_leg_started` banner is a separate push with its own retention, and the
// leg's own `trip_leg_arrived` still arrives.
const pushToStartRetention = 15 * time.Minute

// alertingUpdateRetention is the same day for an ALERTING update, and it is
// deliberately its own constant rather than a reuse of endPushRetention: they
// are two separate decisions about two separate shapes, and a shared constant is
// how one of them silently moves the other.
const alertingUpdateRetention = 24 * time.Hour

// activityExpiration is the apns-expiration instant for one update, and the
// shapes want OPPOSITE things from it.
//
// AN ORDINARY UPDATE EXPIRES AT ITS STALE-DATE. A queued ETA refresh that
// reaches the phone after its content stopped being trustworthy is worse than
// one that never arrives: it overwrites the Activity with a state ActivityKit
// was about to mark stale anyway, resetting the staleness clock on expired
// information. Late is worthless here, so we tell Apple to drop it.
//
// AN ALERTING UPDATE IS THE EXCEPTION, and MYR-413 is what made it one. Since
// the duplicate-banner gate (notifier_activity_gate.go), a rider watching a card
// gets NO lifecycle banner on the phases the island alerts on — so the alerting
// update is now the SOLE carrier of "your car is here" for that rider, and the
// banner it replaced had no apns-expiration at all, which is to say APNs stored
// and retried it. Pinned to a three-minute stale-date, an alerting update to a
// phone in a tunnel is discarded by Apple and the rider reconnects to nothing;
// that is the ordinary case a lock-screen notification exists for, not an edge.
// It gets the same day's floor as an `end` for the same reason.
//
// A late alerting update is SAFE to deliver in a way a late ETA tick is not,
// which is what makes the exception sound rather than merely necessary. The
// stale-date travels IN THE PAYLOAD (buildActivityPayload writes aps.stale-date)
// independent of this header, so an update delivered an hour late self-declares
// as stale and ActivityKit applies its own staleness treatment instead of
// presenting expired data as current; and aps.timestamp ordering means it can
// never overwrite a newer tick that arrived first.
//
// AN `end` MUST OUTLIVE THE PHONE BEING OFFLINE. It is the only push in this
// system with no successor — the rows are tombstoned the moment it is sent, the
// ticker will never look at them again, and nothing retries. Pinned to the
// stale-date it would be discarded by APNs after ~3 minutes, and a rider whose
// phone was in a tunnel when their ride was declined would be left with a lock
// screen reading "your car is on its way" until ActivityKit's own ceiling
// removed it hours later.
//
// It is NOT pinned to the dismissal-date, which is the tempting answer and the
// wrong one. The dismissal-date is 30 SECONDS for the unhappy endings
// (DismissPromptly) — pinning to it would make the most important push in the
// feature the shortest-lived one, worse than the bug being fixed. And an end
// that arrives after its dismissal-date is not wasted: a dismissal-date in the
// past tells iOS to remove the Activity at once, which is exactly the outcome
// wanted for a card that has been lying since the tunnel. So the floor is a
// day, and a dismissal-date is only honoured when it is even later.
//
// Everything is computed off n.Timestamp, not the wall clock, so the header,
// the `aps.timestamp` and the stale-date in the body all describe one instant.
func activityExpiration(n ActivityNotification) time.Time {
	if n.Event == ActivityEventStart {
		// See pushToStartRetention: neither the stale-date nor the end's day is
		// the right horizon for a push that CREATES a card.
		return n.Timestamp.Add(pushToStartRetention)
	}
	if n.Event != ActivityEventEnd {
		if n.Alert == nil {
			return n.StaleDate()
		}
		// A FLOOR, not a replacement: if StaleAfter is ever raised past a day
		// the stale-date is the later of the two and still wins, exactly as the
		// dismissal-date does below.
		retainUntil := n.Timestamp.Add(alertingUpdateRetention)
		if stale := n.StaleDate(); stale.After(retainUntil) {
			return stale
		}
		return retainUntil
	}
	retainUntil := n.Timestamp.Add(endPushRetention)
	if n.DismissalDate != nil && n.DismissalDate.After(retainUntil) {
		return *n.DismissalDate
	}
	return retainUntil
}

// DismissAfter is how long a COMPLETED ride's Activity lingers before iOS
// removes it.
//
// MYR-194: the rider should get to look at the arrival state rather than have
// it vanish the instant the owner taps "Dropped off".
//
// MYR-406 shortened it from fifteen minutes to five, to match the client's own
// completed linger (MYR-405 ends the Activity locally at five). The client's
// timer wins whenever the app is alive; this date is the FALLBACK for a phone
// whose app is dead, so the two disagreeing meant the same ride lingered for
// five minutes or fifteen depending on something the rider cannot see. Five is
// still a long enough look at the arrival state to be worth having, and it is
// gone well before the next ride.
//
// It is deliberately NOT shared with DismissPromptly: the two are separate
// decisions about separate endings, and a shared constant is how one of them
// silently moves the other.
const DismissAfter = 5 * time.Minute

// DismissPromptly is the linger for the unhappy terminal states — declined,
// cancelled, and a reservation that expired.
//
// Not zero, deliberately: an Activity dismissed the same instant it is ended
// can disappear before the rider's eyes reach it, and "my ride vanished" is a
// worse experience than the bad news itself. Thirty seconds is one glance.
const DismissPromptly = 30 * time.Second
