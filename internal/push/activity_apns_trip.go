package push

// THE PUSH-TO-START ATTRIBUTES: the four values that are constant for the life
// of one leg's card, and the type name iOS matches them against.
//
// Split from activity_apns.go so both stay inside the 300-line cap. The seam is
// real: everything in that file is shared by BOTH kinds of Activity — the
// envelope, the priority rule, the expiration rule, the stale-date — while
// these three declarations exist only because a trip leg's card is CREATED by
// the server, which no ride Activity ever is.

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
