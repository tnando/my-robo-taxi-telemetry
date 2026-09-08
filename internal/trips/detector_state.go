package trips

import "time"

// THE RULES: what a SEQUENCE of frames says about whether a car has arrived,
// and the per-car memory that makes a sequence out of deltas.
//
// Separated from detector.go — no I/O, no bus, no store, no clock of its own —
// for exactly the reason internal/arrival separates rules.go: the decision is
// the part a false positive comes out of, and it has to be testable against
// scripted sequences without a database or a car. The frame distillation these
// rules consume is next door in detector_fields.go, which knows nothing about
// history; this file is entirely about history.

// vehicleState is what the detector remembers per VIN between frames: the last
// destination the car reported, and the last fix it can measure stillness from.
//
// Not internally locked. Every method is called from the single handler
// goroutine the bus delivers on, exactly as internal/arrival's tracks are.
type vehicleState struct {
	// destination is the car's current navigation destination name, "" when it
	// has none. Sticky across frames that do not mention it.
	destination string
	// destLat/destLng/hasDest are the destination COORDINATE, when the car
	// streams one. Cleared with the name.
	destLat, destLng float64
	hasDest          bool

	// destClearedAt is when the car FIRST reported an empty destination name
	// while a leg was under way — the start of a PENDING CLEAR, zero when there
	// is none (MYR-612).
	//
	// ⚠ AN EMPTY NAME IS NOT A CLEARED ROUTE, and treating it as one is the bug
	// this field exists to fix. Tesla streams deltas, and on 2026-09-08 a car
	// four minutes into a leg to "Element by Marriott Sedona" sent a frame
	// whose destination name was present-but-empty while `minutesToArrival`
	// still read 98 and the dash still showed the place. The leg closed at
	// 03:40:22 and re-opened at 03:40:24 for the same journey: two legs, two
	// start banners, two push-to-start fan-outs, and leg A's card ended as
	// `completed` on every lock screen that had one.
	//
	// While a clear is pending the last known destination, its coordinate and
	// the arrival latch are all KEPT. Only a clear that is CONFIRMED — see
	// clearConfirmed — actually clears them.
	destClearedAt time.Time
	// etaSeenAt is the last frame that carried an arrival estimate of any kind.
	//
	// It is the second half of the confirmation, and the half that makes the
	// rule fair to a REAL cancellation: a car still reporting how long it has
	// to go is a car that still has somewhere to be, whatever a delta left out
	// of one frame. On a genuine route cancel Tesla stops sending BOTH the name
	// and the estimate, so the two go stale together and the leg closes.
	etaSeenAt time.Time

	// driving is the last decided motion verdict. Sticky across frames that
	// report neither speed nor gear, which is most of them.
	driving bool
	// stoppedSince is when the CURRENT run of stopped frames began, zero while
	// the car is moving or has never reported motion.
	//
	// It is what tells a PARK apart from a red light. `!driving` alone says
	// only that the last frame reported a speed at or under 1 mph, which a car
	// waiting at a junction reports for twenty seconds at a time; using it as
	// the park rung of the clear confirmation made the whole debounce
	// evaporate at the first traffic light after a delta that happened to omit
	// the destination name.
	stoppedSince time.Time
	// prev is the last located fix, held so the positional stillness rung has
	// something to compare against.
	prev *fix
	// stillSince is the start of the current run of qualifying frames — inside
	// the destination radius AND stopped — zero when the car is not currently
	// qualifying. Any disqualifying frame clears it, which is what makes an
	// interrupted dwell restart rather than resume.
	stillSince time.Time
	// arrivalLatched records that this car's current leg has already been
	// closed as arrived, so the twenty further qualifying frames that arrive
	// while it sits there do nothing.
	arrivalLatched bool
	// legID is the leg these per-leg fields are ABOUT, and it is what makes
	// them structurally unable to outlive it.
	//
	// The latch used to be cleared only on a destination NAME change, which is
	// the one clearing condition that does not cover the case the latch is most
	// dangerous in: a second leg to the SAME place — a car that arrives, parks,
	// and later drives back to the same destination, which on a road trip is
	// every return journey. Its name never changes, so the latch survived into
	// the new leg and that leg's arrival branch was disabled for good; the leg
	// could then only ever end as `completed`, and "your car arrived" was never
	// sent. Keyed on the leg id instead, the reset happens because the leg is a
	// different leg, whatever the previous one's ending did.
	legID string

	// lastCardETA and lastCardPush are the CARD THROTTLE. A car streams up to
	// once per second and Apple throttles high-frequency Activity pushes by
	// budget, so a card refresh has to earn its push twice over: the arrival
	// MINUTE must have changed (it is the only number on the card that moves —
	// the destination and the trip name are fixed for the leg), and a minimum
	// interval must have passed.
	//
	// Both conditions, not either. The minute alone would be enough on a
	// motorway and far too much in stop-start traffic, where an ETA can flip
	// between two values every few seconds; the interval alone would push an
	// unchanged card. Held per VIN in memory rather than persisted: losing it
	// on a restart costs one extra push per leg, which is the cheapest possible
	// failure on this surface.
	lastCardETA  *int
	lastCardPush time.Time

	// legRead and legReadAt are the SHORT-LIVED ANSWER to "does this car have
	// an open leg" — the detector's last remaining per-frame database read,
	// held for Config.LegReadTTL (MYR-612).
	//
	// The fact changes at most twice per journey while the question is asked up
	// to once per second per car, on the single bus goroutine. Serving it from
	// memory is what takes the steady state from a sustained query stream to
	// roughly one read per car per TTL; forgetLegRead is called after every
	// write so the frame after an edge sees the truth rather than the picture
	// that prompted it.
	legRead   Leg
	legReadAt time.Time
}

// forgetLegRead invalidates the cached open-leg answer. Called after every leg
// write, so a write is never followed by a decision taken from the state that
// preceded it.
func (v *vehicleState) forgetLegRead() {
	v.legRead, v.legReadAt = Leg{}, time.Time{}
}

// cardMinInterval is the floor between two card refreshes for one car. Chosen
// to sit just under the ride ticker's own 24–36s cadence (MYR-573), which is
// the empirically-survivable rate on this surface: fast enough that a countdown
// reads as continuous, slow enough to stay inside Apple's budget.
const cardMinInterval = 20 * time.Second

// dueForCard reports whether this frame's arrival estimate has earned a push,
// and records the decision when it has.
//
// The FIRST update of a leg always passes (lastCardPush is zero), which is what
// puts a time on a card that was pushed-to-start without one.
func (v *vehicleState) dueForCard(minutes *int, at time.Time) bool {
	if minutes == nil {
		return false
	}
	if v.lastCardETA != nil && *v.lastCardETA == *minutes {
		return false
	}
	if !v.lastCardPush.IsZero() && at.Sub(v.lastCardPush) < cardMinInterval {
		return false
	}
	v.lastCardETA, v.lastCardPush = minutes, at
	return true
}

// beginLeg points the per-leg fields at legID, resetting them when it is a
// different leg from the one they were about.
//
// It is called on every frame that sees an open leg, not only at the edge, so
// the reset cannot be missed by a closing path that failed, by a leg another
// process opened, or by a restart that rebuilt this state from nothing.
func (v *vehicleState) beginLeg(legID string) {
	if v.legID == legID {
		return
	}
	v.legID = legID
	v.arrivalLatched = false
	v.stillSince = time.Time{}
	v.lastCardETA, v.lastCardPush = nil, time.Time{}
}

// endLeg forgets which leg the per-leg fields were about, so the next one
// begins from a clean slate.
func (v *vehicleState) endLeg() { v.beginLeg("") }

// apply folds one frame into the state and reports what changed.
//
// Returns the two edges the detector acts on: whether the car is NOW driving
// with a destination, and whether it just STOPPED being so.
func (v *vehicleState) apply(f fix, cfg Config) {
	if f.minutesToGo != nil || f.milesToGo != nil {
		v.etaSeenAt = f.at
	}
	if f.destName != nil && *f.destName == "" && v.destination != "" {
		// A PENDING CLEAR (MYR-612), not a new destination. Nothing about the
		// remembered route is touched: the name, the coordinate, the arrival
		// latch and the dwell all survive, because a delta that omitted the
		// name is not evidence the car stopped going there. decide decides
		// whether the clear is real; only confirmClear acts on it.
		//
		// ⚠ THE REST OF THIS METHOD STILL RUNS. The motion fold below is what
		// tells the confirmation that the car PARKED, which is one of the three
		// things allowed to settle a clear without waiting out the grace —
		// returning early here would have made a car that stopped and cleared
		// its route in the same frame wait a full minute for a verdict it had
		// already given.
		if v.destClearedAt.IsZero() {
			v.destClearedAt = f.at
		}
	} else if f.destName != nil {
		next := *f.destName
		// A name is BACK: whatever pending clear was running is over, and the
		// leg that would have been closed by it never was.
		v.destClearedAt = time.Time{}
		if next != v.destination {
			// A NEW DESTINATION RESETS THE ARRIVAL STATE. The car is going
			// somewhere else now, so the dwell it may have been accumulating at
			// the old place says nothing about the new one.
			v.stillSince = time.Time{}
			v.arrivalLatched = false
			// A new destination is a new card state. Clearing the throttle
			// lets the next frame carrying an estimate refresh the card at
			// once rather than up to cardMinInterval later.
			v.lastCardETA, v.lastCardPush = nil, time.Time{}
			// AND IT INVALIDATES THE CACHED COORDINATE, which is the half this
			// used to get wrong. Tesla streams deltas, so the frame that
			// announces a new destination NAME very often carries no
			// DestLocation — the coordinate arrives a frame or several later,
			// or never. Left in place, destLat/destLng still point at the
			// PREVIOUS destination while `destination` names the new one, and
			// inRadius prefers the coordinate over the car's own
			// milesToArrival: the detector would then measure arrival against
			// a place the car is no longer going, declaring an arrival the
			// moment it happens to pass the old one and never at the new.
			//
			// Cleared, inRadius falls through to milesToArrival — which on a
			// leg is the most direct evidence there is (see arrivedAt) — until
			// a fresh DestLocation lands. The clear is unconditional on a name
			// change; the `if f.hasDest` below re-arms it in the same frame
			// when the car did send both.
			v.hasDest, v.destLat, v.destLng = false, 0, 0
		}
		v.destination = next
	}
	if f.hasDest {
		v.destLat, v.destLng, v.hasDest = f.destLat, f.destLng, true
	}

	switch reportedMotion(f, cfg.StoppedSpeedMPH) {
	case motionMoving:
		v.driving = true
		v.stoppedSince = time.Time{}
	case motionStopped:
		if v.driving || v.stoppedSince.IsZero() {
			// The START of a stop. A run already under way keeps its original
			// stamp, which is what makes the run a DURATION rather than a
			// restarting instant.
			v.stoppedSince = f.at
		}
		v.driving = false
	case motionUnreported:
		// Left alone deliberately. A frame that says nothing about motion is
		// not evidence that the car stopped — which is the exact cell of the
		// truth table MYR-563 found wrong in the arrival detector.
	}
}

// motion is what a frame's REPORTED fields say about movement. Three-valued,
// because "did not say" is not "said no" (MYR-563).
type motion int

const (
	motionUnreported motion = iota
	motionStopped
	motionMoving
)

// reportedMotion reads the two fields that speak for themselves, in the same
// order and with the same asymmetry as internal/arrival: a reported speed
// decides on its own, gear decides only in its absence, and neither present
// means nothing is claimed.
func reportedMotion(f fix, maxSpeedMPH float64) motion {
	if f.speed != nil {
		if *f.speed <= maxSpeedMPH {
			return motionStopped
		}
		return motionMoving
	}
	switch f.gear {
	case "":
		return motionUnreported
	case gearPark:
		return motionStopped
	default:
		return motionMoving
	}
}

// gearPark is Tesla's shift-state string for park, as normalised by the decoder.
const gearPark = "P"
