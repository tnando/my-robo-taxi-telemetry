//go:build contract

// Contract conformance for MYR-608's `DriveSummary.tripId` (rest-api.md §7.2).
//
// END TO END THROUGH THE REAL SERVER, because `tripId` is a composition no unit
// test holds at once: a Drive row, a trip window, a standing share, and the
// caller's ROLE deciding which of the three statements resolves the field.
// Every piece can be right on its own while the composition names the wrong
// trip — or names one the caller may not read.
//
// WHAT THIS ASSERTS THAT THE STORE TESTS CANNOT: the wire spelling. `tripId` is
// ALWAYS PRESENT AND NULLABLE, deliberately not the omit-when-empty convention
// the four location keys on `DriveSummary` follow, and the whole value of that
// decision lives in the encoded JSON — past the mask, past the projection,
// which is exactly where this test looks.
//
// ⚠ §7.30's OWN SURFACES ARE NOT EXERCISED HERE, and the reason is the harness
// rather than the coverage. This server's trips handler needs the whole
// fifteen-method `TripStore`, whose adapter lives in `cmd/telemetry-server`
// (package main) and is therefore unreachable from this package; mounting it
// would mean a second copy of that adapter maintained here. The §7.30.7 stamp
// and the two `Trip` totals are covered against a real Postgres in
// `internal/store/trip_drive_totals_test.go` and on the wire in
// `internal/telemetry/trip_totals_wire_test.go`. What §7.2 has that neither of
// those has is the REAL role resolution, which is the half this file exists for.
package contract_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestContract_DriveSummaryCarriesTripID(t *testing.T) {
	ctx := context.Background()

	const (
		ownerID    = "user_owner_608"
		riderID    = "user_participant_608"
		vehicleID  = "veh_608_001"
		vehicleVIN = "5YJ3E1EA1PF000601"
		shareID    = "sh_608_inside"
		tripID     = "ctrip_608_window"
	)

	// A window over one afternoon, already closed. ENDED rather than active,
	// because an ended window still admits its participant to its drives (§5)
	// and because the totals must be assertable against a fixed set of rows.
	windowFrom := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	windowTo := time.Date(2026, 8, 12, 20, 0, 0, 0, time.UTC)

	srv, seeder := setupTestServer(t)
	seeder.seedUser(ctx, t, ownerID)
	seeder.seedUser(ctx, t, riderID)
	seeder.seedVehicle(ctx, t, vehicleSeed{
		ID:             vehicleID,
		UserID:         ownerID,
		VIN:            vehicleVIN,
		Name:           "The Bus",
		Model:          "Model Y",
		Year:           2024,
		Color:          "UltraRed",
		Status:         "parked",
		ChargeLevel:    64,
		EstimatedRange: 210,
	})
	seeder.seedAcceptedShare(ctx, t, shareID, vehicleID, ownerID, riderID)
	seeder.seedTrip(ctx, t, tripSeed{
		ID:        tripID,
		VehicleID: vehicleID,
		OwnerID:   ownerID,
		StartsAt:  windowFrom,
		EndsAt:    windowTo,
	})
	seeder.seedTripParticipant(ctx, t, tripID, riderID, shareID)

	// Two drives inside the window and one the day before. The outside drive is
	// what proves `tripId` is a WINDOW test and not "this car has a trip".
	seeder.seedDrive(ctx, t, driveSeed{
		ID: "cdrv_608_in_1", VehicleID: vehicleID, Date: "2026-08-12",
		StartTime: "2026-08-12T13:00:00Z", EndTime: "2026-08-12T14:00:00Z",
		DistanceMiles: 40.5, DurationMinutes: 60,
	})
	seeder.seedDrive(ctx, t, driveSeed{
		ID: "cdrv_608_in_2", VehicleID: vehicleID, Date: "2026-08-12",
		StartTime: "2026-08-12T16:30:00Z", EndTime: "2026-08-12T17:00:00Z",
		DistanceMiles: 19.5, DurationMinutes: 30,
	})
	seeder.seedDrive(ctx, t, driveSeed{
		ID: "cdrv_608_out", VehicleID: vehicleID, Date: "2026-08-11",
		StartTime: "2026-08-11T09:00:00Z", EndTime: "2026-08-11T09:30:00Z",
		DistanceMiles: 100, DurationMinutes: 200,
	})

	t.Run("the owner's §7.2 list names their own window, and null outside it", func(t *testing.T) {
		rows := drivesFor(t, srv, "/api/vehicles/"+vehicleID+"/drives", mintToken(t, ownerID, nil))
		if len(rows) != 3 {
			t.Fatalf("owner was served %d drives, want the car's 3", len(rows))
		}
		byID := indexDrives(rows)

		for _, id := range []string{"cdrv_608_in_1", "cdrv_608_in_2"} {
			if got := byID[id]["tripId"]; got != tripID {
				t.Errorf("%s.tripId = %v, want %q — it began inside the window", id, got, tripID)
			}
		}

		// ALWAYS PRESENT, EXPLICITLY NULL. A missing key would be
		// indistinguishable from a server that does not send the field at all,
		// and a consumer grouping drives under trips would silently fall back
		// to ungrouped against a server that simply had no window to name.
		value, present := byID["cdrv_608_out"]["tripId"]
		if !present {
			t.Fatalf("a drive outside every window has no tripId KEY; the contract declares it "+
				"required and nullable: %v", byID["cdrv_608_out"])
		}
		if value != nil {
			t.Errorf("cdrv_608_out.tripId = %v, want null — it began the day before the window", value)
		}
	})

	t.Run("a participant's §7.2 list is narrowed, and every row it returns is stamped", func(t *testing.T) {
		// The participant reads the SAME endpoint and receives only the
		// window's drives. Being on the page is what makes the id resolvable,
		// so a null here would mean the two halves of one statement disagreed.
		rows := drivesFor(t, srv, "/api/vehicles/"+vehicleID+"/drives", mintToken(t, riderID, nil))
		if len(rows) != 2 {
			t.Fatalf("participant was served %d drives, want the window's 2", len(rows))
		}
		for _, row := range rows {
			if got := row["tripId"]; got != tripID {
				t.Errorf("%v.tripId = %v, want %q on every row a participant is served",
					row["id"], got, tripID)
			}
		}
	})
}

// drivesFor issues one drives request and returns the decoded `items`.
func drivesFor(t *testing.T, srv *httptest.Server, path, token string) []map[string]any {
	t.Helper()
	resp := doGET(t, srv, path, token)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200. Body: %s", path, resp.StatusCode, body)
	}
	var envelope struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
	return envelope.Items
}

// indexDrives keys a page of drive rows by id.
func indexDrives(rows []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(string)
		out[id] = row
	}
	return out
}
