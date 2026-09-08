package push

import (
	"context"
	"log/slog"
)

// The `trips` fan-out (MYR-602, rest-api.md §7.19.0 / §7.30).
//
// FIVE EVENTS, ONE CATEGORY, ONE ENTRY POINT. Unlike every other notifier in
// this package the trips sends are NOT driven by an event-bus subscription:
// their triggers are a database sweep (a window opening or closing), an HTTP
// handler (somebody added to a trip) and a telemetry-derived leg edge, and
// each of those already knows exactly who to tell. So internal/trips calls
// NotifyTrip directly with a resolved audience, and this file's whole job is
// to put those recipients through the SAME gate, device lookup, APNs feedback
// handling and log discipline that every ride push goes through.
//
// WHY IT REUSES delivery/fanOut RATHER THAN GROWING A SECOND SEND PATH. The
// preference gate, the "declare a category or declare yourself transactional"
// refusal, the 410-drops-the-device correction and the P1 logging rules are all
// implemented once, in fanOut. A parallel path for trips would be a second
// place for each of them to be wrong, and the MYR-592 comment on
// delivery.transactional is the standing evidence that these rules do drift
// when they are copied.

// TripEvent names one of the five `trips` notifications. The string values are
// contract-visible: they travel to the app as the `event` userInfo key and are
// what the client switches on to pick a screen, so they are not renameable.
type TripEvent string

const (
	// TripEventAdded — the owner put this person on a trip.
	TripEventAdded TripEvent = "trip_added"
	// TripEventStarted — the trip's window opened.
	TripEventStarted TripEvent = "trip_started"
	// TripEventEnded — the window closed, or the owner ended the trip early.
	TripEventEnded TripEvent = "trip_ended"
	// TripEventLegStarted — inside the window, the car set off for a place.
	TripEventLegStarted TripEvent = "trip_leg_started"
	// TripEventLegArrived — the car reached that place, with arrival evidence.
	TripEventLegArrived TripEvent = "trip_leg_arrived"
)

// TripPush is one trips notification: what happened, on which trip, and to
// whom.
//
// A struct rather than five positional parameters for the reason `delivery`
// gives about its own shape: the leg events differ from the lifecycle ones in
// two fields at once, and a positional string is how a leg id ends up in the
// vehicle slot.
type TripPush struct {
	// TripID is the trip the notification is about. Required — a trips push
	// with no trip has no deep link and nothing to collapse on.
	TripID string
	// VehicleID is the car, carried to the app so it can open the right
	// vehicle without a second call, and used here to resolve the nickname the
	// copy interpolates.
	VehicleID string
	// Event is which of the five this is.
	Event TripEvent
	// LegID is the driving leg, set on the two leg events and empty on the
	// three lifecycle ones.
	//
	// It never reaches the payload — the app has no use for a leg id — and
	// exists only to make the collapse key per-leg. Without it two consecutive
	// legs of one trip would present the same `apns-collapse-id` and Apple
	// would MERGE their banners, so a participant who missed the first arrival
	// would find only the second in Notification Center.
	LegID string
	// DestinationName is where the leg is going, P1, set on the two leg events.
	// It reaches both the copy and the payload; see copy_trips.go for the
	// argued carve-out that permits it on this surface.
	DestinationName string
	// UserIDs is the resolved audience — participants, and the owner on the leg
	// events. Duplicates and empties are tolerated and skipped.
	UserIDs []string
	// Deleted marks a `trip_ended` that is really a DELETION (MYR-607,
	// rest-api.md §7.30.10). It reaches the payload as `deleted: true` and is
	// omitted entirely when false.
	//
	// IT RIDES `trip_ended` RATHER THAN BEING A SIXTH EVENT, and that is a
	// contract decision rather than an economy. Every installed build switches
	// on `event`, and a value they have never seen would route to their default
	// arm — which for a lifecycle push is "do nothing" — so a deleted trip
	// would silently stay on the phone of exactly the people it was deleted
	// out from under. `trip_ended` is already the sentence they act on: the
	// window is over, drop the live card. `deleted` only tells a build that
	// KNOWS about it to go one step further and take the trip out of the list
	// as well, instead of leaving an ended one behind.
	//
	// Set only on the lifecycle end. A leg push never carries it; a leg belongs
	// to a trip that is being deleted whole, and its card is ended by the
	// settlement before this banner goes out.
	Deleted bool
}

// deepLink is the `myrobotaxi://trips/{tripId}` route the app opens when the
// banner is tapped.
func (t TripPush) deepLink() string { return "myrobotaxi://trips/" + t.TripID }

// collapseSubject is what this push de-duplicates against (MYR-554): the trip,
// narrowed to the leg on the two events that have one. See LegID.
func (t TripPush) collapseSubject() string {
	if t.LegID == "" {
		return t.TripID
	}
	return t.TripID + "|" + t.LegID
}

// topic is the fan-out's own topic string, used for the collapse id and every
// log line. It is NOT an events.Topic — there is no bus topic behind these
// sends — and it is spelled with the same dotted shape so an operator grepping
// `topic=` sees one vocabulary.
func (t TripPush) topic() string { return "trip." + string(t.Event) }

// userInfo renders the payload keys outside `aps` (rest-api.md §7.19.0).
//
// `destinationName` is present ONLY on the two leg events and only when it is
// known, following the same absent-means-unknown discipline the content-state
// uses: an empty string would be a value the client has to special-case.
//
// `deleted` follows the same discipline for the same reason: it is written as
// the boolean `true` on a deletion (MYR-607) and OMITTED otherwise, never sent
// as `false`. Absent means "an ordinary end", which is what every push before
// MYR-607 was, so their payloads are unchanged to the byte.
func (t TripPush) userInfo() map[string]any {
	info := map[string]any{
		"tripId":    t.TripID,
		"vehicleId": t.VehicleID,
		"event":     string(t.Event),
		"deepLink":  t.deepLink(),
	}
	if t.DestinationName != "" {
		info["destinationName"] = t.DestinationName
	}
	if t.Deleted {
		info["deleted"] = true
	}
	return info
}

// NotifyTrip delivers one trips notification to every recipient in p.
//
// SYNCHRONOUS, on the caller's context and the caller's goroutine — unlike
// every bus-driven handler in this package, which hands work to `async` so the
// bus's serial per-subscriber loop is not blocked. There is no bus here to
// block: the callers are a 60-second sweeper pass, an HTTP handler that has
// already answered, and a leg edge on the detector's own worker, all of which
// own their contexts and none of which is on a latency path. Running inline
// also means the sweeper can stamp `started_notified_at` AFTER the fan-out
// returns rather than racing it.
//
// BEST-EFFORT THROUGHOUT, like everything else in this package: it returns
// nothing, and a failure to reach one phone never affects the trip, the window,
// or the other recipients.
func (n *Notifier) NotifyTrip(ctx context.Context, p TripPush) {
	if p.TripID == "" || len(p.UserIDs) == 0 {
		return
	}
	a, ok := tripAlert(p.Event, n.vehicleName(ctx, p.VehicleID), p.DestinationName)
	if !ok {
		// An event with no copy. Unreachable through the five constants, and
		// logged rather than silently dropped because the only way to get here
		// is a sixth event added to internal/trips without copy — which would
		// otherwise be a notification nobody ever receives and nobody notices.
		n.logger.Error("push: trips event has no copy; nothing sent",
			slog.String("event", string(p.Event)),
			slog.String("trip_id", p.TripID),
		)
		return
	}

	base := delivery{
		tripPush: &p,
		topic:    p.topic(),
		category: CategoryTrips,
		// islandAlerts stays FALSE. The MYR-413 gate defers a banner to an
		// island that is about to announce the same news, and it is keyed
		// (ride, user) — a trip leg's Live Activity is anchored to a LEG, so
		// the lookup would find nothing and the marking could only ever be a
		// lie. The two surfaces are deliberately both sent here: the banner is
		// what wakes a phone that never registered a push-to-start token, and a
		// leg fires at most twice per journey rather than six times per ride.
	}

	seen := make(map[string]struct{}, len(p.UserIDs))
	for _, userID := range p.UserIDs {
		if userID == "" {
			continue
		}
		if _, dup := seen[userID]; dup {
			// An owner who is also listed as a participant, or a caller that
			// concatenated two reads. Cheap to absorb here; a duplicate would
			// buzz one phone twice for one fact.
			continue
		}
		seen[userID] = struct{}{}

		d := base
		d.userID = userID
		n.fanOut(ctx, d, a)
	}
}
