package push

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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
