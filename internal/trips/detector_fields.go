package trips

import (
	"math"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// THE FRAME, DISTILLED — what one telemetry frame says about a car — plus the
// arithmetic and the field readers it is built from.
//
// Split from detector_state.go so both stay inside the 300-line cap, along the
// seam that already existed: this file is a total function of one frame, with
// no knowledge of a leg, a trip or a car's history. That is exactly what makes
// the RULES beside it — which are all about history — testable against
// scripted sequences.
//
// EVERY FIELD OF `fix` IS NULLABLE BECAUSE A FRAME CARRIES WHATEVER THE CAR
// CHOSE TO SEND. Tesla streams deltas: a frame with a new speed carries no
// destination, and a frame announcing a destination carries no position. That
// one property is why the detector keeps a per-car cache at all, and the whole
// design follows from it.

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
