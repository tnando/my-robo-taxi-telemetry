package push

import "time"

// The TRIP-LEG content state (MYR-602, contracts v0.41.0).
//
// ONE CARD, PARAMETERISED — not a second card. A trip leg and a ride leg are
// the same thing on a lock screen: a car going somewhere, with a name, a time
// and a track. `status` already carries the leg's state, `destination` already
// carries where it is headed, `eta` already carries when it arrives, and `asOf`
// already says when that was true. What a leg needed that a ride did not is
// WHICH KIND OF JOURNEY THIS IS and WHICH TRIP IT BELONGS TO IN WORDS, because
// a participant may hold cards for two shared cars at once and "Optimus · Grand
// Canyon Village" does not say whose road trip that is.
//
// So the wire cost is TWO OPTIONAL KEYS on the existing ContentState, `v` stays
// 1, and every existing key keeps its type, its name and its meaning. An
// installed pre-v0.41.0 build decodes a leg's payload unchanged and renders it
// as the ride card it already knows — the correct degradation, because every
// field it reads still means what it meant.

// ActivityKind is the content-state's `kind` discriminator.
type ActivityKind string

const (
	// ActivityKindRide is the rider's own booked ride. It is the ZERO VALUE and
	// is never sent: absent means ride, permanently, which is what lets a build
	// compiled before v0.41.0 keep decoding this payload.
	ActivityKindRide ActivityKind = "ride"
	// ActivityKindTrip is one driving leg of a shared trip.
	ActivityKindTrip ActivityKind = "trip"
)

// The three status values a trip leg's card ever carries, out of the eight
// LiveActivityRideStatus members. The other five are answers about a ride
// REQUEST, of which a trip has none — nobody accepts or declines a leg.
const (
	// tripStatusEnroute — the car is driving to the leg's destination. A leg
	// spends nearly all its life here.
	tripStatusEnroute = "enroute"
	// tripStatusArrived — the leg ended WITH arrival evidence: the 80 m / 20 s
	// dwell detector fired at the destination.
	tripStatusArrived = "arrived"
	// tripStatusCompleted — the leg ended WITHOUT arrival evidence: the car
	// parked somewhere else, or its navigation was cleared mid-leg. The
	// distinction is load-bearing and is the same one that decides whether the
	// `trip_leg_arrived` push fires at all.
	tripStatusCompleted = "completed"
)

// TripLegContext is everything a leg's content-state needs that is not the
// token: what the card says, as opposed to which card it goes to.
//
// It is the leg-side twin of RideContext, and it is DELIBERATELY SMALLER. Four
// of RideContext's fields exist to answer questions a leg does not raise —
// which leg of a two-leg ride this is (`DispatchUnderway`, `PickupDispatchedAt`),
// whether the named place is a stop (`Stops`), and how far along a dispatched
// pickup the car is (`TripMilesRemaining`, via the progress derivation). A leg
// has one leg, one destination and no dispatch.
type TripLegContext struct {
	// LegID is the leg this card is about — the go_live_activities anchor, and
	// what the send path logs.
	LegID string
	// TripID and VehicleID are the ATTRIBUTES half: static for the life of the
	// card, sent once in the push-to-start and never again. See
	// tripActivityAttributes for why they are not in the content-state.
	TripID    string
	VehicleID string
	// TripName is the owner's label for the window. P1 user content, capped at
	// the schema's 60 and carried ONLY on this surface — never in an alert body
	// or a push title, where copy.go's policy forbids user content.
	TripName string
	// VehicleName is the car's nickname, "" when it has none.
	VehicleName string
	// Destination is where the car said it was going when the leg began. P1.
	Destination string
	// Status is one of the three constants above.
	Status string
	// ETAMinutes is the car's carried navigation ETA in whole minutes, nil when
	// it reports no route — which is the ordinary state at the moment a leg
	// ends, and the reason `eta` is omitted rather than zeroed.
	ETAMinutes *int
	// AsOf is when the values above were last true. See
	// ActivityContentState.AsOf for why it must not re-stamp to `now` on a pass
	// that learned nothing.
	AsOf time.Time
}

// MaxTripNameRunes bounds `tripName`, matching `maxLength: 60` in
// schemas/live-activity.schema.json and Trip.name's own cap.
//
// Enforced HERE at the content-state build rather than trusted to the create
// endpoint, for the same reason MaxContentStateLabel is: this is the one place
// every send path passes through, it also covers names already in the database,
// and Apple's rejection for an over-4KB payload is a flat 400 with no route
// back to the cause that would take out the whole card — every subsequent ETA
// tick included — rather than just the long field.
const MaxTripNameRunes = 60

// tripContentState builds the content-state for one leg push.
//
// `progress` IS DELIBERATELY ABSENT, and it is the one ride field a leg does
// not inherit. The ride derivation (activity_progress.go) measures a fraction
// of a leg whose BOTH ENDS the server knows: it anchors on the dispatched
// pickup or the booked dropoff and clamps monotonically against a stored
// per-Activity floor. A trip leg has no booked endpoints at all — the car's
// driver chose the destination on the dash, mid-leg, and may change it — so the
// only baseline available would be "the remaining distance the first time we
// saw this leg", which moves whenever the route does and would draw a track
// that goes BACKWARDS on a re-route. An absent `progress` renders a card with
// no track, which is a first-class state the client already handles for every
// ride whose car has no active route.
func tripContentState(tc TripLegContext, now time.Time) ActivityContentState {
	state := ActivityContentState{
		Version:     ActivityContentStateVersion,
		Status:      tc.Status,
		VehicleName: truncateLabel(tc.VehicleName),
		Destination: truncateLabel(tc.Destination),
		Kind:        ActivityKindTrip,
		TripName:    truncateRunes(tc.TripName, MaxTripNameRunes),
	}
	if tc.ETAMinutes != nil {
		// ABSOLUTE, like the ride card's: a duration decays silently on a
		// screen the server cannot repaint, whereas an instant stays true no
		// matter how late it is read.
		eta := now.Add(time.Duration(*tc.ETAMinutes) * time.Minute).Unix()
		state.ETA = &eta
	}
	asOf := tc.AsOf
	if asOf.IsZero() {
		// A leg whose state rests on an ASSERTION rather than on a telemetry
		// reading — the arrival, the park, the window closing — was computed
		// now, so `now` is the honest answer. The zero value is never sent:
		// see ActivityContentState.AsOf on why "true in January 1970" is worse
		// than nothing.
		asOf = now
	}
	stamp := asOf.Unix()
	state.AsOf = &stamp
	return state
}
