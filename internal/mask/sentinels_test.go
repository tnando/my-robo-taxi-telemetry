package mask

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// Tripwires for the sentinel-substitution exception (MYR-602). Every one of
// them exists because the exception is the ONE place this package emits a key
// the role may not read, and an exception that is not pinned is an exception
// that grows.

// vehicleStateRequired reads the `required` array of the vendored VehicleState
// schema — the contract statement these tests are bound to. Contracts v0.41.0
// deliberately did NOT relax it while narrowing `viewer`, which is the whole
// reason sentinels exist.
func vehicleStateRequired(t *testing.T) []string {
	t.Helper()

	path := filepath.Join(contractsRoot(t), "docs", "contracts", "schemas", "vehicle-state.schema.json")
	data, err := os.ReadFile(path) //nolint:gosec // test-only read of a repo-relative contract schema
	if err != nil {
		t.Fatalf("read vehicle-state.schema.json: %v", err)
	}
	var doc struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if len(doc.Required) == 0 {
		t.Fatal("read zero required fields from vehicle-state.schema.json — the walk is broken")
	}
	return doc.Required
}

// TestViewerStateSentinelsCoverExactlyTheRequiredGap is the binding assertion.
//
// The set of schema-REQUIRED fields the plain-viewer allow-list withholds must
// equal the set of sentinels, exactly — in both directions:
//
//   - A required field withheld with NO sentinel is the bug this file was
//     written to prevent: the key vanishes and every installed decoder rejects
//     the whole frame, taking down the fields the viewer IS entitled to.
//   - A sentinel for a field that is NOT required (or that the viewer is
//     allowed) is a fabricated value with no contract forcing it — a lie the
//     schema would have let us simply omit.
//
// Binding it to the vendored schema rather than to a hand-written list is what
// makes a future contract change land here: adding `etaMinutes` to `required`
// upstream fails this test until someone decides what a viewer is told.
func TestViewerStateSentinelsCoverExactlyTheRequiredGap(t *testing.T) {
	viewerMask := For(ResourceVehicleState, auth.RoleViewer)

	var gap []string
	for _, field := range vehicleStateRequired(t) {
		if !viewerMask.allows(field) {
			gap = append(gap, field)
		}
	}
	sort.Strings(gap)

	sentinels := make([]string, 0, len(viewerMask.Sentinels))
	for field := range viewerMask.Sentinels {
		sentinels = append(sentinels, field)
	}
	sort.Strings(sentinels)

	if len(gap) != len(sentinels) {
		t.Fatalf("required-but-withheld = %v, sentinels = %v — the two sets must be equal", gap, sentinels)
	}
	for i := range gap {
		if gap[i] != sentinels[i] {
			t.Fatalf("required-but-withheld = %v, sentinels = %v — the two sets must be equal", gap, sentinels)
		}
	}
	if len(gap) == 0 {
		t.Fatal("no required field is withheld from a viewer — either the narrowing was reverted or the schema was relaxed; delete the sentinels rather than leaving them dormant")
	}
}

// TestElevatedRolesNeedNoSentinels pins the corollary. `ride_member` and
// `trip_participant` are allowed every required field, so a sentinel on either
// would be substituting for something they can actually see.
func TestElevatedRolesNeedNoSentinels(t *testing.T) {
	for _, role := range auth.LiveLocationRoles() {
		roleMask := For(ResourceVehicleState, role)
		for _, field := range vehicleStateRequired(t) {
			if !roleMask.allows(field) {
				t.Errorf("role %s is denied schema-required field %q", role, field)
			}
		}
		if len(roleMask.Sentinels) != 0 {
			t.Errorf("role %s carries sentinels %v but is allowed every required field", role, roleMask.Sentinels)
		}
	}
}

// TestSentinelsNeverOverlapTheAllowList forbids the contradictory mask. A name
// in both maps has an unstated precedence; Apply resolves it (Allowed wins) but
// a reader should never have to know that, and the state means somebody
// classified one field twice.
func TestSentinelsNeverOverlapTheAllowList(t *testing.T) {
	for resource, byRole := range masksByResource {
		for role, m := range byRole {
			for field := range m.Sentinels {
				if m.allows(field) {
					t.Errorf("%s/%s: %q is both allowed and sentinel-substituted", resource, role, field)
				}
			}
		}
	}
}

// TestSentinelSubstitutionIsPresenceConditional pins the property that keeps
// this safe on a `vehicle_update` DELTA.
//
// A snapshot carries every field, so all six are substituted and the document
// is schema-complete. A delta carries only what changed, and a mask that
// MANUFACTURED the six keys would turn every delta into a full frame — telling
// a viewer's client the car had just stopped at Null Island every time the
// charge level ticked.
func TestSentinelSubstitutionIsPresenceConditional(t *testing.T) {
	viewerMask := For(ResourceVehicleState, auth.RoleViewer)

	t.Run("absent stays absent", func(t *testing.T) {
		out, _ := Apply(map[string]any{"chargeLevel": 71}, viewerMask)
		if _, present := out["speed"]; present {
			t.Fatalf("delta acquired a speed key it never carried: %v", out)
		}
		if len(out) != 1 {
			t.Fatalf("expected the one allowed key, got %v", out)
		}
	})

	t.Run("present is substituted, not passed through", func(t *testing.T) {
		out, masked := Apply(map[string]any{
			"latitude":  32.7767,
			"longitude": -96.797,
			"speed":     64,
		}, viewerMask)

		for field, want := range map[string]any{"latitude": float64(0), "longitude": float64(0), "speed": 0} {
			got, present := out[field]
			if !present {
				t.Fatalf("%s was removed; the schema requires it", field)
			}
			if got != want {
				t.Fatalf("%s = %v, want the no-value sentinel %v", field, got, want)
			}
		}

		// The audit trail records a withholding, not a disclosure.
		maskedSet := map[string]bool{}
		for _, f := range masked {
			maskedSet[f] = true
		}
		for _, f := range []string{"latitude", "longitude", "speed"} {
			if !maskedSet[f] {
				t.Errorf("%s was substituted but not reported in fieldsMasked", f)
			}
		}
	})
}

// TestSentinelSubstitutionIsIdempotent pins the property Apply's contract
// already promised: projecting an output through the same mask again changes
// nothing. A sentinel that re-substituted a DIFFERENT value on the second pass
// would break every caller that re-projects (the hub does, on replay).
func TestSentinelSubstitutionIsIdempotent(t *testing.T) {
	viewerMask := For(ResourceVehicleState, auth.RoleViewer)

	once, _ := Apply(map[string]any{"latitude": 32.7767, "locationName": "Home", "chargeLevel": 71}, viewerMask)
	twice, _ := Apply(once, viewerMask)

	if len(once) != len(twice) {
		t.Fatalf("second pass changed the key set: %v then %v", once, twice)
	}
	for k, v := range once {
		if twice[k] != v {
			t.Fatalf("second pass changed %q from %v to %v", k, v, twice[k])
		}
	}
	if twice["locationName"] != "" {
		t.Fatalf("locationName = %v, want the empty-string sentinel", twice["locationName"])
	}
}

// TestIsSubstantiveExcludingSentinels is the LIVE BROADCAST half of the
// sentinel story, and the two predicates are tested together because the bug
// they guard against is choosing the wrong one.
//
// Apply substitutes the required location fields in place, so a viewer's
// projection of a GPS-only delta is three constants rather than an empty map.
// On the connect-time snapshot that is the whole point — it is the client's one
// delivery of the required keys. On a LIVE DELTA the same three constants say
// nothing while the frame's ARRIVAL says the car is streaming right now, which
// is the MYR-435 timing leak rebuilt out of zeros.
func TestIsSubstantiveExcludingSentinels(t *testing.T) {
	viewerMask := For(ResourceVehicleState, auth.RoleViewer)

	tests := []struct {
		name string
		// input is the pre-mask payload, projected through the VIEWER mask so
		// the test exercises the real substitution rather than a hand-built map.
		input map[string]any
		// wantLive is the live-broadcast answer (sentinels excluded); wantSnap
		// is the connect-time snapshot answer (sentinels count).
		wantLive bool
		wantSnap bool
	}{
		{
			name:     "location-only delta: the snapshot states it, the stream suppresses it",
			input:    map[string]any{"latitude": 37.7, "longitude": -122.4, "speed": float64(65)},
			wantLive: false,
			wantSnap: true,
		},
		{
			name:     "mixed delta: substantive either way, and the sentinels ride along",
			input:    map[string]any{"latitude": 37.7, "chargeLevel": float64(82)},
			wantLive: true,
			wantSnap: true,
		},
		{
			name: "freshness-only frame is suppressed on both surfaces (MYR-435), and " +
				"the sentinel exclusion must not have quietly changed that",
			input:    map[string]any{"lastUpdated": "2026-09-06T00:00:00Z", "interiorTemp": float64(21)},
			wantLive: false,
			wantSnap: false,
		},
		{
			name:     "owner-only cabin group projects to nothing at all",
			input:    map[string]any{"interiorTemp": float64(21)},
			wantLive: false,
			wantSnap: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projected, fieldsMasked := Apply(tt.input, viewerMask)

			if got := IsSubstantiveExcludingSentinels(projected, fieldsMasked); got != tt.wantLive {
				t.Errorf("IsSubstantiveExcludingSentinels = %v, want %v (projection %v, masked %v)",
					got, tt.wantLive, projected, fieldsMasked)
			}
			if got := IsSubstantive(projected); got != tt.wantSnap {
				t.Errorf("IsSubstantive = %v, want %v (projection %v)", got, tt.wantSnap, projected)
			}
		})
	}
}

// TestIsSubstantiveExcludingSentinels_OwnerIsUnaffected pins the property that
// keeps the new predicate from being a second, quieter mask table: for a role
// that is withheld nothing, fieldsMasked is empty and the two predicates are
// the same function.
func TestIsSubstantiveExcludingSentinels_OwnerIsUnaffected(t *testing.T) {
	input := map[string]any{"latitude": 37.7, "longitude": -122.4}
	projected, fieldsMasked := Apply(input, For(ResourceVehicleState, auth.RoleOwner))

	if len(fieldsMasked) != 0 {
		t.Fatalf("the owner mask withheld %v; this test's premise is gone", fieldsMasked)
	}
	if !IsSubstantiveExcludingSentinels(projected, fieldsMasked) {
		t.Error("an owner's real coordinates were read as sentinels and suppressed")
	}
}

// TestWithheldIsByteIdenticalToNoFix asserts the property the whole exception
// rests on, DIRECTLY rather than by construction.
//
// Every other test here proves the sentinels are the right SET and are
// substituted at the right TIMES. This one proves the thing a consumer actually
// depends on: a viewer's projection of a car that IS reporting a position is
// byte-for-byte the same document as a viewer's projection of a car that has no
// fix at all. If it ever stopped being true, a client could distinguish
// "withheld" from "no GPS" by reading the bytes — and the moment one can, the
// narrowing leaks the one bit it exists to withhold: whether the car is
// somewhere.
//
// rest-api.md §5 states the other half as an obligation ON THE CLIENT: it must
// branch on the role it holds (`role` + `activeTripId` + `hasActiveRide`),
// because the value cannot tell it. That obligation is only reasonable if this
// test passes.
func TestWithheldIsByteIdenticalToNoFix(t *testing.T) {
	viewerMask := For(ResourceVehicleState, auth.RoleViewer)

	// A car parked outside somebody's house, reporting everything.
	located := map[string]any{
		"speed":           42,
		"heading":         180,
		"latitude":        37.7749,
		"longitude":       -122.4194,
		"locationName":    "Home",
		"locationAddress": "123 Market St, San Francisco, CA",
		"chargeLevel":     78,
		"status":          "parked",
	}
	// The SAME car with no fix and no geocode — the state vehicle-state-schema
	// §2.3 documents, which every consumer already handles because a car that
	// has never reported a position emits it.
	noFix := map[string]any{
		"speed":           0,
		"heading":         0,
		"latitude":        float64(0),
		"longitude":       float64(0),
		"locationName":    "",
		"locationAddress": "",
		"chargeLevel":     78,
		"status":          "parked",
	}

	withheld, _ := Apply(located, viewerMask)
	honest, _ := Apply(noFix, viewerMask)

	// MARSHALLED, not DeepEqual'd: the wire is what a consumer sees, and two
	// maps that compare equal in Go could still differ on the wire if a
	// sentinel's TYPE were wrong (a float where the schema says integer is as
	// much a decode failure as an absent key).
	gotWithheld, err := json.Marshal(withheld)
	if err != nil {
		t.Fatalf("marshal withheld: %v", err)
	}
	gotHonest, err := json.Marshal(honest)
	if err != nil {
		t.Fatalf("marshal no-fix: %v", err)
	}
	if string(gotWithheld) != string(gotHonest) {
		t.Errorf("a viewer can tell WITHHELD from NO FIX by reading the bytes:\n"+
			" withheld: %s\n  no-fix: %s\n"+
			"The two must be indistinguishable — a client is told to branch on its ROLE "+
			"precisely because the value cannot tell it, and any difference here leaks "+
			"whether the car is somewhere.", gotWithheld, gotHonest)
	}

	// And the catalog floor survives both: a narrowing takes location, never
	// the whole document.
	if withheld["chargeLevel"] != 78 || withheld["status"] != "parked" {
		t.Errorf("the narrowing took more than location: %v", withheld)
	}
}
