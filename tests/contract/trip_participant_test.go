//go:build contract

// Contract conformance for the MYR-602 TRIP_PARTICIPANT role (rest-api.md §5,
// §7.1, §7.30).
//
// WHAT THIS COVERS THAT NOTHING ELSE COULD UNTIL NOW. The harness provisioned
// the trips tables but not `go_vehicle_shares` or `go_ride_members`, so
// auth.queryUserVehicleIDs — four UNION legs naming all four relations in one
// statement — could not run here at all, and the harness comment recorded that
// a participant-admission test was therefore unwritable. Both tables are now
// provisioned, and this is the test that was owed.
//
// It is deliberately end-to-end through a REAL handler with the REAL
// authenticator's ResolveRole, because the property is a JOIN of four things no
// unit test holds at once: the standing share, the trip membership, the WINDOW,
// and the mask the resolved role selects. Every one of them can be right in
// isolation while the composition is wrong.
package contract_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestContract_TripParticipantSeesLocation(t *testing.T) {
	ctx := context.Background()

	const (
		ownerID     = "user_owner_tripc"
		insideID    = "user_participant_tripc"
		outsideID   = "user_plainviewer_tripc"
		vehicleID   = "veh_tripc_001"
		vehicleVIN  = "5YJ3E1EA1PF000401"
		locName     = "Home"
		locAddr     = "123 Market St, San Francisco, CA"
		wantLat     = 37.7749
		wantLng     = -122.4194
		insideShare = "sh_tripc_inside"
	)

	tests := []struct {
		name string
		// who reads the snapshot.
		userID string
		// wantLocated is whether the reader is entitled to the car's real
		// position — which is exactly "is this person inside an open window".
		wantLocated bool
		why         string
	}{
		{
			name:        "a participant inside an open window reads the real coordinate",
			userID:      insideID,
			wantLocated: true,
			why: "a trip is a window during which the owner's chosen share-holders see " +
				"the car live; if this fails the feature does nothing",
		},
		{
			name:        "a plain share-holder reads the no-fix sentinel",
			userID:      outsideID,
			wantLocated: false,
			why: "MYR-602 narrowed `viewer` to catalog and availability; a standing share " +
				"is no longer a licence to watch somebody drive",
		},
		{
			name:        "the owner reads the real coordinate",
			userID:      ownerID,
			wantLocated: true,
			why:         "the narrowing is about non-owners; an owner sees their own car",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, seeder := setupTestServer(t)
			for _, id := range []string{ownerID, insideID, outsideID} {
				seeder.seedUser(ctx, t, id)
			}
			seeder.seedVehicle(ctx, t, vehicleSeed{
				ID:              vehicleID,
				UserID:          ownerID,
				VIN:             vehicleVIN,
				Name:            "Optimus",
				Model:           "Model 3",
				Year:            2024,
				Color:           "Solid Black",
				Status:          "parked",
				ChargeLevel:     78,
				EstimatedRange:  245,
				Latitude:        wantLat,
				Longitude:       wantLng,
				LocationName:    locName,
				LocationAddress: locAddr,
			})

			// BOTH non-owners hold the SAME standing grant. That is the whole
			// point of the case: what separates them is the trip, and nothing
			// else — no second grant, no different permission, no new vehicle
			// relationship. A trip decides what an EXISTING share MEANS between
			// two instants.
			seeder.seedAcceptedShare(ctx, t, insideShare, vehicleID, ownerID, insideID)
			seeder.seedAcceptedShare(ctx, t, "sh_tripc_outside", vehicleID, ownerID, outsideID)

			now := time.Now().UTC()
			seeder.seedTrip(ctx, t, tripSeed{
				ID:        "ctrip_tripc",
				VehicleID: vehicleID,
				OwnerID:   ownerID,
				StartsAt:  now.Add(-time.Hour),
				EndsAt:    now.Add(time.Hour),
			})
			seeder.seedTripParticipant(ctx, t, "ctrip_tripc", insideID, insideShare)

			resp := doGET(t, srv, "/api/vehicles/"+vehicleID+"/snapshot", mintToken(t, tt.userID, nil))
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
			}

			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode snapshot: %v", err)
			}

			// THE SIX REQUIRED FIELDS ARE ALWAYS PRESENT, whatever the role.
			// That is the sentinel rule and it is not optional: they are in
			// vehicle-state.schema.json's `required` array, so removing them
			// would not narrow the frame, it would make the whole document
			// undecodable for every installed build — taking chargeLevel and
			// status down with the location that was the only thing at issue.
			for _, key := range []string{
				"speed", "heading", "latitude", "longitude", "locationName", "locationAddress",
			} {
				if _, present := got[key]; !present {
					t.Errorf("%q is absent; the schema declares it REQUIRED, so a consumer "+
						"cannot decode this document at all", key)
				}
			}

			if tt.wantLocated {
				if got["latitude"] != wantLat || got["longitude"] != wantLng {
					t.Errorf("coordinate = (%v, %v), want (%v, %v); %s",
						got["latitude"], got["longitude"], wantLat, wantLng, tt.why)
				}
				if got["locationName"] != locName {
					t.Errorf("locationName = %v, want %q; %s", got["locationName"], locName, tt.why)
				}
				return
			}

			// WITHHELD IS SPELLED AS THE SCHEMA'S OWN NO-VALUE SPELLING —
			// `0,0` for the coordinate (vehicle-state-schema.md §2.3), `""` for
			// the two place strings. A consumer CANNOT tell "withheld" from "no
			// fix" by reading the value; the two are the same bytes by design,
			// and rest-api.md §5 states the obligation to branch on the role.
			if got["latitude"] != float64(0) || got["longitude"] != float64(0) {
				t.Errorf("coordinate = (%v, %v), want the (0,0) no-fix sentinel; %s",
					got["latitude"], got["longitude"], tt.why)
			}
			if got["locationName"] != "" || got["locationAddress"] != "" {
				t.Errorf("place = (%v, %v), want the empty no-geocode spelling; %s",
					got["locationName"], got["locationAddress"], tt.why)
			}
			// THE CATALOG FLOOR SURVIVES. A narrowing takes location, not the
			// whole document — a plain viewer still needs to know the car is
			// there and whether it is available.
			if got["chargeLevel"] != float64(78) {
				t.Errorf("chargeLevel = %v, want 78 — the narrowing was scoped to location",
					got["chargeLevel"])
			}
		})
	}
}

// TestContract_TripParticipantLosesLocationWhenTheWindowCloses is the same
// person, the same grant, one instant later.
//
// A CLOSED WINDOW IS NOT A REVOCATION and nothing is written when it closes:
// `NOW()` simply stops satisfying the predicate. That is why the property has
// to be asserted against a real query rather than reasoned about — there is no
// state change anywhere to inspect.
func TestContract_TripParticipantLosesLocationWhenTheWindowCloses(t *testing.T) {
	ctx := context.Background()

	const (
		ownerID   = "user_owner_tripw"
		riderID   = "user_participant_tripw"
		vehicleID = "veh_tripw_001"
		shareID   = "sh_tripw"
	)

	srv, seeder := setupTestServer(t)
	seeder.seedUser(ctx, t, ownerID)
	seeder.seedUser(ctx, t, riderID)
	seeder.seedVehicle(ctx, t, vehicleSeed{
		ID: vehicleID, UserID: ownerID, VIN: "5YJ3E1EA1PF000402",
		Name: "Optimus", Model: "Model 3", Year: 2024, Color: "Solid Black",
		Status: "parked", ChargeLevel: 78, EstimatedRange: 245,
		Latitude: 37.7749, Longitude: -122.4194,
		LocationName: "Home", LocationAddress: "123 Market St",
	})
	seeder.seedAcceptedShare(ctx, t, shareID, vehicleID, ownerID, riderID)

	now := time.Now().UTC()
	// A window that CLOSED an hour ago. The share is untouched and still
	// accepted; only the clock moved.
	seeder.seedTrip(ctx, t, tripSeed{
		ID: "ctrip_tripw", VehicleID: vehicleID, OwnerID: ownerID,
		StartsAt: now.Add(-3 * time.Hour), EndsAt: now.Add(-time.Hour),
	})
	seeder.seedTripParticipant(ctx, t, "ctrip_tripw", riderID, shareID)

	resp := doGET(t, srv, "/api/vehicles/"+vehicleID+"/snapshot", mintToken(t, riderID, nil))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a lapsed window narrows the reader, it does not "+
			"remove the car from their catalog. Body: %s", resp.StatusCode, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if got["latitude"] != float64(0) || got["longitude"] != float64(0) {
		t.Errorf("coordinate = (%v, %v) after the window closed, want the no-fix sentinel — "+
			"trip access is PURELY the window, and nothing is written when it lapses",
			got["latitude"], got["longitude"])
	}
}

// TestContract_SuspendedShareDefeatsTheTrip is the "trip access cannot outlive
// the share" rule, asserted at the one place it is enforced: every access query
// re-joins the LIVE grant rather than trusting the participant row.
//
// There is nothing to clean up when a share is suspended — the trip row and the
// membership row are both untouched — so if the join were ever dropped the
// participant would keep live location with no trace of why.
func TestContract_SuspendedShareDefeatsTheTrip(t *testing.T) {
	ctx := context.Background()

	const (
		ownerID   = "user_owner_trips"
		riderID   = "user_participant_trips"
		vehicleID = "veh_trips_001"
		shareID   = "sh_trips_susp"
	)

	srv, seeder := setupTestServer(t)
	seeder.seedUser(ctx, t, ownerID)
	seeder.seedUser(ctx, t, riderID)
	seeder.seedVehicle(ctx, t, vehicleSeed{
		ID: vehicleID, UserID: ownerID, VIN: "5YJ3E1EA1PF000403",
		Name: "Optimus", Model: "Model 3", Year: 2024, Color: "Solid Black",
		Status: "parked", ChargeLevel: 78, EstimatedRange: 245,
		Latitude: 37.7749, Longitude: -122.4194,
		LocationName: "Home", LocationAddress: "123 Market St",
	})
	seeder.seedAcceptedShare(ctx, t, shareID, vehicleID, ownerID, riderID)

	now := time.Now().UTC()
	seeder.seedTrip(ctx, t, tripSeed{
		ID: "ctrip_trips", VehicleID: vehicleID, OwnerID: ownerID,
		StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour),
	})
	seeder.seedTripParticipant(ctx, t, "ctrip_trips", riderID, shareID)

	// The window is OPEN and the membership is live. The owner suspends the
	// grant, and nothing else changes anywhere.
	seeder.suspendShare(ctx, t, shareID)

	resp := doGET(t, srv, "/api/vehicles/"+vehicleID+"/snapshot", mintToken(t, riderID, nil))
	body := readBody(t, resp)
	if resp.StatusCode == http.StatusOK {
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		if got["latitude"] != float64(0) || got["longitude"] != float64(0) {
			t.Fatalf("a SUSPENDED share-holder inside an open window still read (%v, %v) — "+
				"trip access outlived the grant it was built on",
				got["latitude"], got["longitude"])
		}
		return
	}
	// A REFUSAL IS EQUALLY CORRECT AND IS WHAT ACTUALLY HAPPENS, because the
	// share gate runs first: ShareGrantFor's statement excludes suspended rows,
	// so the reader is refused before the trip is consulted at all. That is the
	// strongest possible form of the rule — the suspension defeats the trip
	// without any code path having to know a trip exists — and it is why no
	// gate on the platform names suspension.
	//
	// What must not happen, and is the only thing this test forbids, is a 200
	// carrying the coordinate.
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 200-with-sentinels, 404 or 403. Body: %s", resp.StatusCode, body)
	}
}
