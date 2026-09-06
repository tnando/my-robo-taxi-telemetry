package mask

// SENTINEL SUBSTITUTION — the narrow exception to "absent, not nulled".
//
// The allow-list rule this package is built on is that a field the role may
// not see is REMOVED: the JSON key is absent, never present-and-nulled
// (rest-api.md §5.1). That rule is right for every field the schema declares
// OPTIONAL, and it is impossible for a field the schema declares REQUIRED.
//
// MYR-602 created the collision. Narrowing `viewer` took the Speed/GPS group
// off a plain share-holder — `speed`, `heading`, `latitude`, `longitude`,
// `locationName`, `locationAddress` — and all six are in the `required` list
// of schemas/vehicle-state.schema.json, which v0.41.0 did NOT relax. Removing
// them outright produces a VehicleState that fails its own schema, and the
// consumer that breaks is not a validator in CI: it is the decoder inside
// every already-installed build, which decodes a required field into a
// non-optional property and rejects the WHOLE frame when it is missing. The
// viewer's snapshot would not have been narrowed — it would have been
// unreadable, and every field they ARE entitled to (charge, status, name)
// would have gone down with the six.
//
// So for exactly these fields the projection substitutes the schema's OWN
// DOCUMENTED NO-VALUE SPELLING instead of removing the key. This is not an
// invention of this package: vehicle-state-schema.md §2.3 already defines
// `0, 0` as "no GPS fix" for the non-nullable coordinate columns, and the
// schema's own text defines `""` as "no geocode yet" for the two place
// strings. A viewer is told the same thing the server tells everybody when it
// genuinely has no fix — which is, from that caller's side of the mask, true.
//
// WHAT THIS COSTS, STATED PLAINLY: a consumer cannot distinguish "withheld"
// from "the car has no fix" by reading the value, because the two are the same
// bytes by design. The consumer distinguishes them by the ROLE IT HOLDS, which
// it already knows — `VehicleSummary.role` plus `activeTripId` / `hasActiveRide`
// tell it whether it is inside a window. rest-api.md §5 states this obligation
// on the client; a client that ignores it renders a car parked at Null Island
// at 0 mph, which is why the obligation is written down rather than assumed.
//
// SCOPE IS DELIBERATELY MINIMAL. Only `vehicle_state`, only `viewer`, only the
// six required fields. Every OPTIONAL field the narrowing withholds — the whole
// navigation group, `driveTrailCoordinates`, the snapshot aliases — is still
// removed outright, because the schema permits absence there and absence is the
// honest answer. TestViewerStateSentinelsCoverExactlyTheRequiredGap pins the
// two sets against the vendored schema, so a future field added to `required`
// cannot quietly reopen the hole and a field dropped from `required` cannot
// leave a sentinel behind pretending to be data.
//
// The elevated roles need no entry: `ride_member` and `trip_participant` are
// allowed all six, so nothing is substituted for them.

// vehicleStateViewerSentinels maps each schema-REQUIRED field the plain-viewer
// allow-list withholds to the no-value spelling the schema documents for it.
//
// The types matter and are the schema's: `speed` and `heading` are integers,
// the coordinates are numbers, the two place fields are strings. A float where
// the schema says integer is as much a decode failure as an absent key on a
// strictly-typed client, which is the failure this whole file exists to avoid.
var vehicleStateViewerSentinels = map[string]any{
	// Speed/GPS atomic group. §2.3's "0,0 = no fix" convention, extended to
	// the two scalars that travel with the pair — a car reported at no fix is
	// reported as not moving, which is the only self-consistent reading.
	"speed":     0,
	"heading":   0,
	"latitude":  float64(0),
	"longitude": float64(0),
	// The two derived place strings. Non-nullable in the DB (NOT NULL
	// DEFAULT ''), and the schema already defines "" as "no geocode yet".
	"locationName":    "",
	"locationAddress": "",
}

// sentinel reports the substitution value for a withheld-but-required field,
// and whether there is one at all.
//
// PRESENCE-CONDITIONAL AT THE CALL SITE, not here: Apply substitutes only for a
// key the INPUT actually carried. That distinction is what keeps this safe on
// the two very different payloads that flow through the same projection — a
// §7.1 snapshot carries every field, so all six are substituted and the
// response is schema-complete; a `vehicle_update` DELTA carries only what
// changed, so a frame that never mentioned `speed` does not acquire one. A mask
// that manufactured keys would turn every delta into a full frame and would
// tell a viewer's client that the car had just stopped every time the charge
// level ticked.
func (m ResourceMask) sentinel(field string) (any, bool) {
	if m.Sentinels == nil {
		return nil, false
	}
	v, ok := m.Sentinels[field]
	return v, ok
}
