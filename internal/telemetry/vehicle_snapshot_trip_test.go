package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// §7.1 UNDER THE MYR-602 ROLE SPLIT.
//
// The snapshot resolves its mask role through the SAME auth.ResolveVehicleAccess
// every other surface uses, so a caller inside an open trip window arrives here
// as `trip_participant` with nothing in this handler needing to know about
// trips. These tests pin what that role sees, and — just as important — what
// the caller becomes the moment the window closes.
//
// Together with internal/auth/trip_participant_access_test.go (which pins the
// RESOLUTION) this closes the §7.1 half of the feature: that one proves the
// role is reached, these prove it is worth reaching.

// snapshotBodyForRole runs the handler with a fixed role and returns the JSON.
func snapshotBodyForRole(t *testing.T, role auth.Role) map[string]any {
	t.Helper()

	const userID = "usr_snapshot_caller"
	row := fixtureSnapshotRow(userID)
	row.Latitude = 32.7767
	row.Longitude = -96.797
	row.Speed = 64
	row.Heading = 271
	row.LocationName = "I-20 W"
	row.LocationAddress = "Interstate 20, Weatherford TX"
	dest := "Barstow Supercharger"
	row.DestinationName = &dest

	h := NewVehicleSnapshotHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleSnapshotReader{row: row},
		discardLogger(),
		WithSnapshotRoleResolver(&stubRoleResolver{role: role}),
	)
	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/vehicles/"+fixtureSnapshotRowID+"/snapshot", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// TestSnapshotGivesATripParticipantTheLocationAndNavigationGroups.
//
// This IS the feature on §7.1: inside the window the participant sees where the
// car is and where it is going. The assertion is on real values, not on key
// presence, because the narrowed viewer below receives the same KEYS carrying
// sentinels — presence alone would pass for both roles and prove nothing.
func TestSnapshotGivesATripParticipantTheLocationAndNavigationGroups(t *testing.T) {
	body := snapshotBodyForRole(t, auth.RoleTripParticipant)

	for field, want := range map[string]any{
		"latitude":        32.7767,
		"longitude":       -96.797,
		"speed":           float64(64),
		"heading":         float64(271),
		"locationName":    "I-20 W",
		"locationAddress": "Interstate 20, Weatherford TX",
		"destinationName": "Barstow Supercharger",
	} {
		got, present := body[field]
		if !present {
			t.Errorf("%s is missing from the trip_participant projection", field)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want the real value %v", field, got, want)
		}
	}
}

// TestSnapshotOutsideAWindowFallsBackToTheNarrowedViewer.
//
// The other side of the same coin, and the reason MYR-602 is a NARROWING as well
// as a widening: when the window closes the caller is an ordinary viewer again,
// and a viewer no longer receives live location at all.
//
// THE SIX REQUIRED FIELDS KEEP THEIR KEYS carrying the schema's own no-value
// spellings — removing them would make the whole document undecodable to every
// installed client rather than merely narrower (internal/mask/sentinels.go).
// The OPTIONAL navigation fields are absent outright, because the schema permits
// absence there and absence is the honest answer.
func TestSnapshotOutsideAWindowFallsBackToTheNarrowedViewer(t *testing.T) {
	body := snapshotBodyForRole(t, auth.RoleViewer)

	t.Run("the required location fields are present as sentinels", func(t *testing.T) {
		for field, want := range map[string]any{
			"latitude":        float64(0),
			"longitude":       float64(0),
			"speed":           float64(0),
			"heading":         float64(0),
			"locationName":    "",
			"locationAddress": "",
		} {
			got, present := body[field]
			if !present {
				t.Errorf("%s was dropped; the schema declares it required", field)
				continue
			}
			if got != want {
				t.Errorf("%s = %v, want the no-value sentinel %v — the key survives, the value never does", field, got, want)
			}
		}
	})

	t.Run("the optional navigation fields are absent outright", func(t *testing.T) {
		for _, field := range []string{"destinationName", "destinationAddress", "etaMinutes", "navRouteCoordinates"} {
			if v, present := body[field]; present {
				t.Errorf("%s is present (%v) on a plain viewer's snapshot", field, v)
			}
		}
	})

	t.Run("the catalog floor survives the narrowing", func(t *testing.T) {
		// The narrowing was scoped to location and navigation. Taking the car
		// with it would have destroyed the feature the share still buys.
		for _, field := range []string{"vehicleId", "name", "model", "status", "chargeLevel", "estimatedRange"} {
			if _, present := body[field]; !present {
				t.Errorf("%s is missing — the narrowing reached past the location groups", field)
			}
		}
	})
}

// TestSnapshotTripParticipantAndRideMemberSeeTheSameThing pins the identity the
// two window-scoped roles were built on.
//
// They share ONE field list by construction (auth.LiveLocationRoles), and that
// is what makes MYR-540 ride tracking provably unchanged by MYR-602: the same
// list, the same bytes. A divergence here would mean somebody had narrowed one
// of them without deciding to.
func TestSnapshotTripParticipantAndRideMemberSeeTheSameThing(t *testing.T) {
	member := snapshotBodyForRole(t, auth.RoleRideMember)
	participant := snapshotBodyForRole(t, auth.RoleTripParticipant)

	if len(member) != len(participant) {
		t.Fatalf("ride_member has %d fields, trip_participant has %d — the two window-scoped roles must share one list",
			len(member), len(participant))
	}
	for field, want := range member {
		got, present := participant[field]
		if !present {
			t.Errorf("trip_participant is missing %q, which ride_member receives", field)
			continue
		}
		// Compared by VALUE, not just presence: a sentinel substitution applied
		// to one role and not the other would pass a key-set comparison.
		if got != want {
			t.Errorf("%s: ride_member = %v, trip_participant = %v", field, want, got)
		}
	}
}
