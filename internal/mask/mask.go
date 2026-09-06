// Package mask implements the role-based field projection layer that
// governs every SDK-exposed REST response and WebSocket frame. The
// canonical per-resource matrix lives in docs/contracts/rest-api.md §5.2
// and is consumed by both the WebSocket hub's per-role projection
// (docs/contracts/websocket-protocol.md §4.6) and the REST handler-layer
// mask (rest-api.md §5.1). The single source-of-truth for the matrix
// satisfies CG-DC-5 in docs/contracts/data-classification.md §5.
//
// Allow-list semantics are fail-closed:
//   - A field that is not enumerated in the role's mask is OMITTED from
//     the projected output entirely (the JSON key is absent, not nulled).
//     "Absent, not nulled" is required by rest-api.md §5.1; emitting null
//     would leak the existence of the field to the viewer.
//   - An unknown (resource, role) pair returns the zero-value mask, which
//     in turn produces an empty projected output — i.e., deny-all.
//   - The empty Role("") sentinel from auth.ParseRole models "unknown
//     role" and also resolves to deny-all.
//
// Payload values flow through this package as map[string]any. The "any"
// element type is intentional — the mask is schema-less and projects
// arbitrary JSON-decoded payloads (vehicle_update fields, REST response
// bodies). Constraining the value type would force every caller to
// flatten its struct into a typed map, which is more work for no
// safety win at the projection step.
package mask

import (
	"github.com/myrobotaxi/telemetry/internal/auth"
)

// ResourceType identifies the kind of payload being projected. The set
// is closed at v1 — see rest-api.md §5.2 for the canonical resource
// list. Adding a new resource is a one-row change in tables.go.
type ResourceType string

// v1 resource types. Field sets per (resource, role) are defined in
// tables.go.
const (
	ResourceVehicleState   ResourceType = "vehicle_state"
	ResourceVehicleSummary ResourceType = "vehicle_summary"
	ResourceDriveSummary   ResourceType = "drive_summary"
	ResourceDriveDetail    ResourceType = "drive_detail"
	ResourceDriveRoute     ResourceType = "drive_route"
	ResourceInvite         ResourceType = "invite"
)

// ResourceMask is the allow-list of field names visible to a single
// (resource, role) pair. Stored as a set for O(1) membership checks
// inside Apply. The zero value (nil Allowed) represents the deny-all
// mask, which is what fail-closed produces for unknown (resource, role)
// pairs.
//
// ⚠ BOTH MAPS ARE IMMUTABLE AFTER INIT AND MUST NEVER BE WRITTEN TO. `For`
// returns the table's own map by REFERENCE, not a copy, so every caller
// projecting a frame for one (resource, role) pair shares one map — a single
// write would silently change what every later projection on the process
// emits, for every connected client, with nothing in the diff to explain it.
// Reads are race-safe precisely because there are no writes: the tables are
// built once at package init and never touched again. A caller needing a
// variant must build its own map, not edit this one.
type ResourceMask struct {
	// Allowed is the set of field names that should pass through Apply.
	// A nil or empty map produces an empty projected payload regardless
	// of input.
	Allowed map[string]struct{}

	// Sentinels is the SUBSTITUTION table: field names that are NOT allowed
	// but that must nonetheless appear in the output, mapped to the no-value
	// spelling to emit in place of the real one. Nil on almost every mask —
	// see sentinels.go for the single case that needs it (MYR-602's narrowed
	// `viewer` against the six schema-REQUIRED location fields) and for why
	// removing them outright would make the frame undecodable rather than
	// merely narrower.
	//
	// A name in BOTH maps is a contradiction; Allowed wins in Apply and
	// TestSentinelsNeverOverlapTheAllowList forbids the state outright.
	Sentinels map[string]any
}

// allows reports whether the given field name is permitted by this
// mask. A nil Allowed always returns false (deny-all).
func (m ResourceMask) allows(field string) bool {
	if m.Allowed == nil {
		return false
	}
	_, ok := m.Allowed[field]
	return ok
}

// Apply projects input through the mask and returns:
//
//   - out: a new map containing only the keys allowed by mask. Allowed
//     keys map to the same value as in input. Keys absent from mask are
//     omitted from out entirely (no key emitted) — see "absent, not
//     nulled" in rest-api.md §5.1 — UNLESS the mask carries a sentinel for
//     them, in which case the key survives carrying the schema's documented
//     no-value spelling instead of the real one. See sentinels.go: that
//     exception exists only for fields the schema declares `required`, where
//     removing the key does not narrow the frame but makes it undecodable.
//   - fieldsMasked: the names of input keys that were removed by the
//     projection. Used by the audit-log emit path (deferred, see
//     audit.go and the TODOs in the hub / REST handler) to record which
//     fields were stripped on a 1% sample. Stable order is not
//     guaranteed — the audit path treats this as a set.
//
// Apply never mutates input. The returned out is a fresh map; callers
// may safely marshal it without affecting the source payload.
//
// Apply is idempotent: applying the same mask to its own output
// produces an identical map (the second pass finds every key already
// in Allowed and removes nothing).
func Apply(input map[string]any, mask ResourceMask) (out map[string]any, fieldsMasked []string) {
	out = make(map[string]any, len(input))
	for k, v := range input {
		if mask.allows(k) {
			out[k] = v
			continue
		}
		// WITHHELD. The value never survives; the only question is whether
		// the KEY does. It does for the handful of fields the schema declares
		// required (sentinels.go) and for nothing else.
		//
		// Substituted fields are still reported in fieldsMasked, deliberately:
		// the audit trail records what was WITHHELD, and a sentinel is a
		// withholding that happens to leave a key behind. Recording it as
		// visible would make the audit understate the projection.
		if s, ok := mask.sentinel(k); ok {
			out[k] = s
		}
		fieldsMasked = append(fieldsMasked, k)
	}
	return out, fieldsMasked
}

// For returns the ResourceMask for a (resource, role) pair. Unknown
// pairs produce the zero-value (deny-all) mask — see CG-DC-5 in
// data-classification.md §5 and the fail-closed semantics in §5 of
// rest-api.md. Callers MUST NOT special-case the deny-all return value;
// the empty mask is a valid output and Apply handles it correctly.
func For(resource ResourceType, role auth.Role) ResourceMask {
	roleTable, ok := masksByResource[resource]
	if !ok {
		return ResourceMask{}
	}
	mask, ok := roleTable[role]
	if !ok {
		return ResourceMask{}
	}
	return mask
}

// Allows reports whether a field survives this mask.
//
// The exported form of the internal predicate, added by MYR-602 for the ONE
// caller that must consult a mask WITHOUT running a projection: the catalog's
// trip merge stamps `activeTripId` onto rows the earlier merge legs have
// already projected, so it has no unprojected map to pass through Apply.
//
// STAMPING PAST A MASK IS EXACTLY THE PATTERN THIS PACKAGE EXISTS TO PREVENT,
// so the stamp asks first. Consulting the mask keeps the allow-list the single
// authority over that field: remove `activeTripId` from a role's list and the
// stamp stops, rather than continuing to write a key the table no longer
// permits.
func (m ResourceMask) Allows(field string) bool { return m.allows(field) }
