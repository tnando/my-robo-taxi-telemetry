package mask

import (
	"github.com/myrobotaxi/telemetry/internal/auth"
)

// The REQUIRED-FIELD SENTINELS for a role that may not see live location
// (MYR-602, contracts v0.41.0).
//
// THE PROBLEM. This package's whole discipline is "absent, not nulled": a field
// the role may not see has its JSON key OMITTED, because emitting `null` would
// leak the field's existence. That is right for every OPTIONAL field, and it is
// the rule §5.1 states.
//
// MYR-602 narrowed `viewer` so that a standing share no longer carries the
// Speed/GPS group — and six of the fields it took away are `required` in
// vehicle-state.schema.json: `speed`, `heading`, `latitude`, `longitude`,
// `locationName`, `locationAddress`. `VehicleState.required` did NOT change in
// v0.41.0. So a viewer projection that merely dropped them produces an object
// that fails its own schema, and a conformant SDK is entitled to discard the
// whole frame — which would take the catalog fields the narrowing was never
// about (`status`, `chargeLevel`, `estimatedRange`) down with it.
//
// THE ANSWER IS THE ONE THE SCHEMA ALREADY DEFINES. `(0, 0)` is the documented
// NO-FIX SENTINEL (vehicle-state-schema.md §2.3) — a real value on the wire, a
// coordinate ~1,600 km off the Gulf of Guinea that no car is ever at, and one
// every consumer already has to handle because a car that has never reported a
// position emits it. `speed` and `heading` take 0 and the two labels take "",
// which are the same "nothing known" values a never-located car carries. So a
// viewer's frame validates, renders no location, and says nothing about where
// the car is — which is the entire point of the narrowing.
//
// WHAT THIS IS NOT. It is not a way to smuggle a field past a mask: every
// value here is a constant, computed from nothing, identical for every vehicle
// and every instant. Nor does it widen the OPTIONAL navigation group — those
// fields stay absent, because absent is a legal state for them and a sentinel
// destination would be a lie the schema does not force.

// requiredLocationSentinelOrder fixes the field list, and the ORDER is only for
// deterministic logging and tests; the map below is the authority on values.
//
// It is EXACTLY the intersection of vehicle-state.schema.json's `required` list
// with mask.vehicleStateLiveLocationFields, and TestSentinelsCoverEveryRequired
// LocationField pins that identity so a field moving into or out of either list
// cannot silently stop being covered.
var requiredLocationSentinelOrder = []string{
	"speed", "heading", "latitude", "longitude", "locationName", "locationAddress",
}

// requiredLocationSentinels is the value each of those keys takes for a role
// that may not see live location.
//
// The numeric zeros are float64 rather than int: the wire type is `number` and
// the live path's values arrive as float64, so an int here would render `0`
// where the neighbouring real value renders `0` too — identical JSON today, but
// a typed consumer that switches on the decoded Go type would see two shapes
// for one field. One type, always.
var requiredLocationSentinels = map[string]any{
	"speed":           float64(0),
	"heading":         float64(0),
	"latitude":        float64(0),
	"longitude":       float64(0),
	"locationName":    "",
	"locationAddress": "",
}

// NeedsLocationSentinels reports whether a role's vehicle_state projection must
// carry the sentinels.
//
// TRUE FOR EXACTLY ONE ROLE: `viewer`. Owners, ride members and trip
// participants all receive the real values (auth.Role.SeesLiveLocation), and
// the empty Role("") sentinel is NOT covered — deny-all must stay deny-all, and
// sending a stranger a schema-valid frame full of zeros would be a frame where
// the contract says there should be none at all.
func NeedsLocationSentinels(role auth.Role) bool { return role == auth.RoleViewer }

// ApplyLocationSentinels re-inserts the schema-required location keys that the
// mask removed, as sentinels, and reports which ones it filled in.
//
// SCOPED TO KEYS THAT WERE IN THE INPUT. A `vehicle_update` frame is a DELTA —
// it carries the fields that just changed and nothing else — so inventing all
// six keys on a frame that was about the charge level would state "the car is
// at (0,0) as of now" on a frame that made no claim about position at all. The
// keys that were masked away are the ones the frame was about; those, and only
// those, come back as sentinels.
//
// It never OVERWRITES: a key that survived the mask is left exactly as it is,
// so calling this with a role that sees live location (or with a projection
// that already carries the real values) is a no-op rather than a redaction.
//
// The input map is not mutated. `projected` is mutated in place and returned,
// because it is already a fresh map Apply made for this caller.
func ApplyLocationSentinels(
	input, projected map[string]any,
	role auth.Role,
) (out map[string]any, filled []string) {
	if !NeedsLocationSentinels(role) {
		return projected, nil
	}
	for _, field := range requiredLocationSentinelOrder {
		if _, present := input[field]; !present {
			continue
		}
		if _, survived := projected[field]; survived {
			continue
		}
		projected[field] = requiredLocationSentinels[field]
		filled = append(filled, field)
	}
	return projected, filled
}

// RequiredLocationSentinelFields returns the covered key list, defensively
// copied. Exported so the per-surface conformance tests in internal/ws and
// internal/telemetry assert against ONE list rather than three hand-copied
// ones — the same reason OwnerOnlyVehicleStateFields is exported.
func RequiredLocationSentinelFields() []string {
	out := make([]string, len(requiredLocationSentinelOrder))
	copy(out, requiredLocationSentinelOrder)
	return out
}
