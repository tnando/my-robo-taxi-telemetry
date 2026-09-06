package trips

import (
	"math"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// The pure half of the leg detector: what one telemetry frame says about a car,
// and what a sequence of them says about whether it has arrived.
//
// Separated from detector.go — no I/O, no bus, no store, no clock of its own —
// for exactly the reason internal/arrival separates rules.go: the decision is
// the part a false positive comes out of, and it has to be testable against
// scripted sequences without a database or a car.

// fix is one observation distilled from a frame.
//
// EVERY FIELD IS NULLABLE BECAUSE A FRAME CARRIES WHATEVER THE CAR CHOSE TO
// SEND. Tesla streams deltas: a frame with a new speed carries no destination,
// and a frame announcing a destination carries no position. That is why the
// detector keeps a per-VIN CACHE (vehicleState) rather than deciding from one
// frame — the whole design follows from this one property.
//
// The coordinates are P1 GPS data — never logged.
type fix struct {
	at        time.Time
	lat, lng  float64
	hasFix    bool
	speed     *float64
	gear      string
	destName  *string
	destLat   float64
	destLng   float64
	hasDest   bool
	milesToGo *float64
	// minutesToGo is the car's own arrival estimate, which is the ONE number on
	// the leg card that moves. Nullable for the reason every field here is: a
	// frame carries what the car chose to send.
	minutesToGo *float64
}

// fixFrom distils a telemetry frame.
//
// It NEVER returns "nothing usable": unlike internal/arrival, which needs a
// position to measure and skips a frame without one, this detector cares about
// frames that carry only a DESTINATION — a car whose driver just set a route on
// the dash while parked sends exactly that, and it is the frame that opens a
// leg. The caller decides what a frame is good for.
func fixFrom(te events.VehicleTelemetryEvent) fix {
	f := fix{at: te.CreatedAt}

	if loc := locationField(te.Fields, telemetry.FieldLocation); loc != nil {
		// The (0, 0) NO-FIX SENTINEL is rejected here for the same reason
		// internal/arrival rejects it: it is a real value on the wire that
		// decodes as an ordinary coordinate ~1,600 km off the Gulf of Guinea.
		if loc.Latitude != 0 || loc.Longitude != 0 {
			f.lat, f.lng, f.hasFix = loc.Latitude, loc.Longitude, true
		}
	}
	if loc := locationField(te.Fields, telemetry.FieldDestLocation); loc != nil {
		if loc.Latitude != 0 || loc.Longitude != 0 {
			f.destLat, f.destLng, f.hasDest = loc.Latitude, loc.Longitude, true
		}
	}
	f.speed = floatField(te.Fields, telemetry.FieldSpeed)
	f.gear = stringField(te.Fields, telemetry.FieldGear)
	f.milesToGo = floatField(te.Fields, telemetry.FieldMilesToArrival)
	f.minutesToGo = floatField(te.Fields, telemetry.FieldMinutesToArrival)

	if v, ok := te.Fields[string(telemetry.FieldDestinationName)]; ok && !v.Invalid && v.StringVal != nil {
		// PRESENT-BUT-EMPTY IS THE SIGNAL THAT CLEARS A ROUTE, and it must be
		// distinguishable from ABSENT, which says nothing. A pointer to "" is
		// "the driver cancelled navigation"; a nil pointer is "this frame was
		// about something else". Collapsing them would end a leg on every
		// speed-only frame.
		name := *v.StringVal
		f.destName = &name
	}
	return f
}

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

	// driving is the last decided motion verdict. Sticky across frames that
	// report neither speed nor gear, which is most of them.
	driving bool
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

// apply folds one frame into the state and reports what changed.
//
// Returns the two edges the detector acts on: whether the car is NOW driving
// with a destination, and whether it just STOPPED being so.
func (v *vehicleState) apply(f fix, cfg Config) {
	if f.destName != nil {
		next := *f.destName
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
		}
		v.destination = next
		if next == "" {
			v.hasDest = false
		}
	}
	if f.hasDest {
		v.destLat, v.destLng, v.hasDest = f.destLat, f.destLng, true
	}

	switch reportedMotion(f, cfg.StoppedSpeedMPH) {
	case motionMoving:
		v.driving = true
	case motionStopped:
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

// arrivedAt folds one frame into the dwell and reports whether the car has now
// been stopped at its destination for the whole window.
//
// THE DISTANCE COMES FROM THE COORDINATE WHEN THE CAR STREAMS ONE, and from the
// car's own `milesToArrival` when it does not. That second source is the exact
// signal internal/arrival refuses, and refusing it there is right for the reason
// its package doc gives — the dash's target and the RIDE's target are different
// facts. ON A LEG THEY ARE THE SAME FACT BY CONSTRUCTION: a leg is defined as
// "the car is driving to the place the dash names", so the dash's distance to
// that place is the most direct evidence there is.
//
// STILLNESS IS STILL REQUIRED either way, which is what stops a leg "arriving"
// at a red light 60 m short of the destination, and the dwell is measured on
// FRAME timestamps so a burst of backlogged frames cannot fake it.
func (v *vehicleState) arrivedAt(f fix, cfg Config) bool {
	prev := v.prev
	if f.hasFix {
		snapshot := f
		v.prev = &snapshot
	}

	if !v.inRadius(f, cfg) {
		v.stillSince = time.Time{}
		return false
	}
	since, still := v.stillnessRun(f, prev, cfg)
	if !still {
		v.stillSince = time.Time{}
		return false
	}
	if v.stillSince.IsZero() {
		v.stillSince = since
	}
	// Not Before rather than After, so a zero dwell (tests) fires on the first
	// qualifying frame and a frame landing exactly on the boundary counts.
	return !f.at.Before(v.stillSince.Add(cfg.Dwell))
}

// inRadius reports whether this frame places the car at its destination.
func (v *vehicleState) inRadius(f fix, cfg Config) bool {
	if v.hasDest && f.hasFix {
		return haversineMeters(f.lat, f.lng, v.destLat, v.destLng) <= cfg.ArrivalRadiusMeters
	}
	if f.milesToGo != nil {
		return *f.milesToGo*metersPerMile <= cfg.ArrivalRadiusMeters
	}
	// No coordinate and no reported distance: nothing is claimed, which is the
	// direction every ambiguity in this detector resolves to.
	return false
}

// stillnessRun answers "is this car stopped, and if so since when", with the
// same three-rung ladder internal/arrival uses — a reported speed, then gear,
// then positional stillness across a bounded interval (MYR-563).
//
// The third rung matters here for the same reason it matters there and MORE: a
// car that parks STOPS STREAMING, so the frames that prove it stayed put are
// REST-backfill fixes carrying a location and nothing else.
func (v *vehicleState) stillnessRun(f fix, prev *fix, cfg Config) (time.Time, bool) {
	switch reportedMotion(f, cfg.StoppedSpeedMPH) {
	case motionStopped:
		// A reported stillness speaks for THIS instant only.
		return f.at, true
	case motionMoving:
		return time.Time{}, false
	case motionUnreported:
	}

	if prev == nil || !f.hasFix {
		return time.Time{}, false
	}
	gap := f.at.Sub(prev.at)
	if gap <= 0 || gap > cfg.MaxStillnessGap {
		return time.Time{}, false
	}
	if haversineMeters(prev.lat, prev.lng, f.lat, f.lng) > cfg.StillnessMeters {
		return time.Time{}, false
	}
	// Stillness proven across the interval, so the dwell may honestly count
	// from the EARLIER fix.
	return prev.at, true
}

// metersPerMile converts the car's reported `milesToArrival`.
const metersPerMile = 1609.344

// haversineMeters returns the great-circle distance in meters.
//
// Package-private and duplicated from internal/arrival, internal/geocode and
// internal/store for the reason those already give each other: importing any of
// them would invert an established dependency direction to save nine lines of
// arithmetic.
func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusMeters = 6_371_000.0
	const deg2rad = math.Pi / 180.0

	dLat := (lat2 - lat1) * deg2rad
	dLng := (lng2 - lng1) * deg2rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*deg2rad)*math.Cos(lat2*deg2rad)*
			math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthRadiusMeters * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// locationField returns a frame's location field, nil when absent or invalid.
func locationField(fields map[string]events.TelemetryValue, name telemetry.FieldName) *events.Location {
	v, ok := fields[string(name)]
	if !ok || v.Invalid {
		return nil
	}
	return v.LocationVal
}

// floatField returns a nullable float field. Nullable rather than (value, ok)
// because "the car did not say" has to survive into the rules: absent speed is
// not zero speed.
func floatField(fields map[string]events.TelemetryValue, name telemetry.FieldName) *float64 {
	v, ok := fields[string(name)]
	if !ok || v.Invalid {
		return nil
	}
	return v.FloatVal
}

// stringField returns a string field, empty when absent or invalid.
func stringField(fields map[string]events.TelemetryValue, name telemetry.FieldName) string {
	v, ok := fields[string(name)]
	if !ok || v.Invalid || v.StringVal == nil {
		return ""
	}
	return *v.StringVal
}
