package push

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// The ActivityKit half of the APNs client (MYR-172).

// activityAPS is the `aps` dictionary of an ActivityKit remote update.
//
// The key names are Apple's and are hyphenated; they are pinned here as struct
// tags rather than assembled into a map so that a typo is a compile-time
// rename rather than a push Apple answers 400 to. Field order in the struct is
// the order they appear in the rendered JSON, which makes the payload readable
// in a packet capture.
type activityAPS struct {
	// Timestamp is `aps.timestamp` — when the state was true, in unix seconds.
	// ActivityKit drops an update whose timestamp is older than the one it is
	// already showing, which is the only defence against a reordered pair of
	// pushes leaving the lock screen on the stale one.
	Timestamp int64 `json:"timestamp"`

	// Event is `update` or `end`.
	Event ActivityEvent `json:"event"`

	// ContentState is the payload the Swift ContentState decodes.
	ContentState ActivityContentState `json:"content-state"`

	// StaleDate is `aps.stale-date` in unix seconds — past it, ActivityKit
	// renders the Activity as stale on its own. Always set: an update with no
	// stale-date is a promise we cannot keep.
	StaleDate int64 `json:"stale-date"`

	// DismissalDate is `aps.dismissal-date` in unix seconds, present only on an
	// end event. Omitted means iOS dismisses the Activity immediately.
	DismissalDate *int64 `json:"dismissal-date,omitempty"`

	// AttributesType is `aps.attributes-type`, present ONLY on a `start` event
	// (MYR-602). It names the Swift `ActivityAttributes` struct iOS must
	// instantiate to create the Activity, and it must match the type name in
	// the widget bundle EXACTLY — a mismatch is silently ignored by the device,
	// with APNs answering 200 and no card ever appearing. It is
	// `TripActivityAttributes`; see TripActivityAttributesType.
	AttributesType string `json:"attributes-type,omitempty"`

	// Attributes is `aps.attributes`, the STATIC half of the Activity, present
	// only on a `start`. It is decoded once into the Swift attributes struct
	// and never changes for the life of the card, which is exactly why these
	// values are NOT in the content-state: re-sending `tripId` and `vehicleId`
	// on every ETA tick would spend Apple's 4KB budget on constants.
	Attributes *tripActivityAttributes `json:"attributes,omitempty"`

	// Alert is `aps.alert`, present only on the six phase changes (MYR-398) and
	// only ever on an `update` (MYR-418 — see buildActivityPayload).
	// Its PRESENCE is the whole mechanism — iOS expands the Dynamic Island for
	// ~3s on an update that carries one — so an empty dictionary would be an
	// unintended expansion rather than a harmless default, and the pointer is
	// nil on every other push.
	//
	// Last in the struct, so the four keys that shipped before it hold their
	// positions in a packet capture.
	Alert *activityAlert `json:"alert,omitempty"`
}

// activityAlert is the `aps.alert` dictionary.
//
// Deliberately title/body LITERALS rather than `title-loc-key`/`loc-key`. The
// localised form would keep the copy in the app where §7.21.3 says copy
// belongs, but a key the installed build's string table does not carry renders
// as the RAW KEY on the lock screen — and a server that ships ahead of the app
// is the normal state of this project, not the exceptional one. See
// ActivityAlert for the payload-policy rules the strings obey.
//
// No `sound`. The design asks for an EXPANSION, not an interruption: six
// beeps per ride from a surface whose whole premise is that it replaces eleven
// notifications would undo the feature. An alert dictionary with no sound
// expands the island silently.
type activityAlert struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// TripActivityAttributesType is the value of `aps.attributes-type` on a
// trip-leg push-to-start, and it is a CROSS-REPOSITORY CONSTANT: it must be the
// exact name of the `ActivityAttributes`-conforming struct in the iOS widget
// bundle. iOS matches it by name to decide which Activity to instantiate, and a
// mismatch fails SILENTLY — APNs answers 200, the device drops the push, and no
// card ever appears, with no signal on either side. Changing it is an iOS
// change and a server change in the same release, never one of the two.
const TripActivityAttributesType = "TripActivityAttributes"

// tripActivityAttributes is `aps.attributes` for a trip-leg push-to-start: the
// three values that are constant for the life of one leg's card.
//
// WHY THESE FOUR AND NOT MORE. The attributes are decoded ONCE and can never
// be updated, so a value belongs here only if it cannot change while the card
// is alive. `tripId` and `vehicleId` are identifiers the widget needs for its
// deep link and cannot derive. `vehicleName` is the owner's nickname, which
// CAN in principle change mid-leg — it is here anyway because the card must be
// legible the instant it appears, before any content-state update arrives, and
// a nickname edit during a single leg is not a case worth spending a required
// content-state key on. The content-state carries it too, so an update
// corrects it.
//
// `legId` IS WHAT MAKES THE CARD ADDRESSABLE AT ALL, and it is the key without
// which the whole update path is one-way. ActivityKit hands the app a
// per-Activity UPDATE token the moment the card is created, and the server has
// nowhere to file it: registration is anchored on the LEG (§7.21.7,
// go_live_activities.trip_leg_id), and the device cannot derive which leg its
// card is for — it was asleep when the leg opened, and a trip has many legs.
// Without it the server can create a card and never update or end it, so every
// leg's card would run to ActivityKit's own ceiling still saying the car was
// driving somewhere it reached hours before.
//
// It qualifies on the same never-changes rule the other three meet, exactly and
// not conveniently: an Activity IS one leg. A new leg is a new card, and a card
// whose leg ended is ended.
//
// The TRIP NAME is deliberately NOT here even though it is equally static: it
// is P1 user content, and the content-state already carries it under a key the
// client reads on every push. One place, one classification argument.
type tripActivityAttributes struct {
	TripID string `json:"tripId"`
	// LegID is REQUIRED, not optional, and the requirement is the iOS side's:
	// `TripActivityAttributes` declares it non-optional, so a payload missing
	// the key FAILS THE DECODE and no card appears at all. That is the
	// deliberate direction — a card with no anchor could never be updated or
	// ended, and a silently un-endable card on a lock screen is worse than no
	// card. No `omitempty`, therefore, on this or on any of its neighbours:
	// every one of them is a value the widget needs before its first update.
	//
	// P0 — an opaque server-minted id, like the two below it. Placed second,
	// beside the trip it narrows.
	LegID       string `json:"legId"`
	VehicleID   string `json:"vehicleId"`
	VehicleName string `json:"vehicleName"`
}

// TripActivityStart is the addressing half of one push-to-start: which phone,
// and which Activity to create on it. All four fields become attributes, and
// all four are REQUIRED on the wire — see tripActivityAttributes.
type TripActivityStart struct {
	TripID string
	// LegID is the anchor the created card registers its own update token
	// against (§7.21.7). Without it the card can never be updated or ended.
	LegID     string
	VehicleID string
	// VehicleName is the nickname the card shows before its first update.
	VehicleName string
}

// activityPayload is the whole APNs body.
//
// Unlike the alert payload beside it, there is NO userInfo: an Activity update
// carries no ride id outside the content-state, because the token itself
// already addresses exactly one Activity for exactly one ride. Adding a ride id
// would be a P0 identifier on the wire buying nothing.
type activityPayload struct {
	APS activityAPS `json:"aps"`
}

// buildActivityPayload renders the APNs JSON body for a Live Activity update.
func buildActivityPayload(n ActivityNotification) ([]byte, error) {
	aps := activityAPS{
		Timestamp:    n.Timestamp.Unix(),
		Event:        n.Event,
		ContentState: n.ContentState,
		StaleDate:    n.StaleDate().Unix(),
	}
	if n.Event == ActivityEventEnd && n.DismissalDate != nil {
		dismiss := n.DismissalDate.Unix()
		aps.DismissalDate = &dismiss
	}
	// A `start` — and ONLY a start — carries the attributes (MYR-602). Written
	// here, at the one place the keys are either present or not, for the same
	// reason the `end`-never-alerts rule is enforced here: this surface has no
	// failure signal, so attributes on an `update` would be accepted by APNs,
	// ignored by the device, and indistinguishable from working.
	if n.Event == ActivityEventStart && n.Start != nil {
		aps.AttributesType = TripActivityAttributesType
		aps.Attributes = &tripActivityAttributes{
			TripID:      n.Start.TripID,
			LegID:       n.Start.LegID,
			VehicleID:   n.Start.VehicleID,
			VehicleName: n.Start.VehicleName,
		}
	}
	// AN `end` NEVER RENDERS AN ALERT, whatever the caller asked for (MYR-418).
	//
	// Apple's ActivityKit push documentation introduces the alert dictionary
	// under `start` and `update`; of an `end` it says only to "include the final
	// content state". An alert there is undocumented — and, on the client's
	// real-device ride, accepted by APNs and honoured by nothing: the island
	// never expanded and the rider saw no arrival at all.
	//
	// Enforced HERE, at the one place the key is either written or not, rather
	// than trusted to the callers. That is a deliberate answer to how this
	// defect survived: there is no failure signal anywhere on this surface — no
	// 400, no reason string, no metric — so an alert that does nothing looks
	// exactly like an alert that worked, from the server all the way to the
	// logs. A caller that wants the sixth expansion must send it on the alerting
	// UPDATE that precedes the end, which is what endRide does.
	// A `start` may alert, and Apple's own documentation introduces the alert
	// dictionary under `start` and `update` — but no trip caller sets one. The
	// card APPEARING is the announcement, and the `trip_leg_started` banner is
	// already on its way from the ordinary notifier; a third simultaneous
	// interruption for one fact is what MYR-413 exists to stop.
	if n.Alert != nil && n.Event != ActivityEventEnd {
		aps.Alert = &activityAlert{Title: n.Alert.Title, Body: n.Alert.Body}
	}

	body, err := json.Marshal(activityPayload{APS: aps})
	if err != nil {
		return nil, fmt.Errorf("push: marshal activity payload: %w", err)
	}
	return body, nil
}

// activityTopic derives the Live Activity topic from the app's bundle id.
//
// Apple requires the `.push-type.liveactivity` suffix on the topic AND the
// matching apns-push-type header; either alone is rejected with
// TopicDisallowed, which is a 403 that reads like a credential problem and is
// not one. Deriving the suffix here rather than adding a second config value
// means the two topics cannot drift apart in an environment file.
func activityTopic(bundleTopic string) string {
	return bundleTopic + liveActivityTopicSuffix
}

// SendActivity delivers one ActivityKit remote update, retrying once on a
// network error or 5xx, and maps APNs rejections onto ErrUnregistered /
// ErrThrottled exactly as Send does.
//
// ErrUnregistered here means the ACTIVITY is gone — dismissed by the rider,
// ended by the app, or expired — not that the phone is gone. The caller drops
// the go_live_activities row, and the device registry is untouched.
func (c *Client) SendActivity(ctx context.Context, n ActivityNotification) error {
	body, err := buildActivityPayload(n)
	if err != nil {
		return err
	}

	return c.deliver(ctx, apnsMessage{
		deviceToken: n.ActivityToken,
		sandbox:     n.Sandbox,
		pushType:    pushTypeLiveActivity,
		topic:       activityTopic(c.topic),
		priority:    n.priority(),
		expiration:  strconv.FormatInt(activityExpiration(n).Unix(), 10),
		body:        body,
	})
}

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
