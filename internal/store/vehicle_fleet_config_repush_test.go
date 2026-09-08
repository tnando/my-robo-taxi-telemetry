package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_ListStreamingFleetConfigVehicles backs the MYR-630 re-push
// sweep, and it is the same kind of safety boundary as the MYR-448 candidate
// query in the file next door: everything this returns is a REAL car that a
// sweep may push a new config at.
//
// The properties are symmetric and both matter:
//
//   - it must NAME every car that holds a live config, or a field change stays
//     dormant on the cars it omits and nothing will ever notice;
//   - it must EXCLUDE, in SQL, the two cars nobody may push at — a tombstoned
//     one and an unacknowledged driver-access one — because those are consent
//     decisions, not operator preferences;
//   - and it must REPORT, rather than drop, the cars the sweep will refuse for
//     its own reasons, so a dry run can explain every line.
func TestVehicleRepo_ListStreamingFleetConfigVehicles(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	mustApplyOwnerSchema(t)
	cleanTables(t, testPool)
	cleanFleetCandidateTables(t)
	cleanRepushTables(t)

	ctx := context.Background()
	const owner = "user_repush"
	const ownerNoTesla = "user_repush_notesla"
	seedFCCAccount(t, "acct_repush", owner, "tesla-sub-repush")

	recent := time.Now().Add(-2 * time.Minute)
	older := time.Now().Add(-6 * time.Hour)

	// The ordinary case: a car that streams.
	seedFleetCandidateVehicle(t, "veh_rp_live", owner, "5YJ3E1EA1NF000801", "tv_rp_live", recent)
	// Quiet for hours but still configured — a parked car is exactly what the
	// MYR-629 resend is FOR, so it must be listed.
	seedFleetCandidateVehicle(t, "veh_rp_parked", owner, "5YJ3E1EA1NF000802", "tv_rp_parked", older)
	// Suspended for owner inactivity (MYR-592): reported, flagged, refused later.
	seedFleetCandidateVehicle(t, "veh_rp_susp", owner, "5YJ3E1EA1NF000803", "tv_rp_susp", older)
	seedRepushSuspension(t, "veh_rp_susp", time.Now().Add(-24*time.Hour))
	// Warned but NOT suspended: still streaming, still needs the new field set.
	seedFleetCandidateVehicle(t, "veh_rp_warned", owner, "5YJ3E1EA1NF000804", "tv_rp_warned", older)
	seedRepushWarning(t, "veh_rp_warned", time.Now().Add(-2*time.Hour))
	// A config that never landed: reported, flagged, left to the reconciler.
	seedFleetCandidateVehicle(t, "veh_rp_absent", owner, "5YJ3E1EA1NF000805", "tv_rp_absent", older)
	seedRepushAttempt(t, "veh_rp_absent", "awaiting_virtual_key")
	// A schedule row whose outcome does NOT mean the config is missing.
	seedFleetCandidateVehicle(t, "veh_rp_synced", owner, "5YJ3E1EA1NF000806", "tv_rp_synced", older)
	seedRepushAttempt(t, "veh_rp_synced", "synced_but_quiet")
	// Deliberately removed by its owner (MYR-261 tombstone-wins).
	seedFleetCandidateVehicle(t, "veh_rp_tomb", owner, "5YJ3E1EA1NF000807", "tv_rp_tomb", older)
	seedFCCTombstone(t, owner, "tv_rp_tomb", "5YJ3E1EA1NF000807")
	// Linked by a driver who never acknowledged owner approval (MYR-599).
	seedFleetCandidateVehicle(t, "veh_rp_drv", owner, "5YJ3E1EA1NF000808", "tv_rp_drv", older)
	seedRepushDriverAccess(t, "veh_rp_drv", owner, false)
	// Same, acknowledged: an ordinary car from here on.
	seedFleetCandidateVehicle(t, "veh_rp_drv_ok", owner, "5YJ3E1EA1NF000809", "tv_rp_drv_ok", older)
	seedRepushDriverAccess(t, "veh_rp_drv_ok", owner, true)
	// Provisioned before Tesla supplied a VIN — nothing to push against.
	seedFleetCandidateVehicle(t, "veh_rp_novin", owner, "", "tv_rp_novin", older)
	// Owner with no Tesla account row. Deliberately still LISTED: the sweep
	// labels it no_token rather than silently shortening its own report.
	seedFleetCandidateVehicle(t, "veh_rp_notesla", ownerNoTesla, "5YJ3E1EA1NF000810", "tv_rp_notesla", older)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})

	t.Run("lists every pushable car and excludes only the consent cases", func(t *testing.T) {
		rows, err := repo.ListStreamingFleetConfigVehicles(ctx, 100)
		if err != nil {
			t.Fatalf("ListStreamingFleetConfigVehicles: %v", err)
		}
		got := map[string]store.StreamingFleetVehicle{}
		for _, r := range rows {
			got[r.VehicleID] = r
		}

		for _, want := range []string{
			"veh_rp_live", "veh_rp_parked", "veh_rp_susp", "veh_rp_warned",
			"veh_rp_absent", "veh_rp_synced", "veh_rp_drv_ok", "veh_rp_notesla",
		} {
			if _, ok := got[want]; !ok {
				t.Errorf("%s missing — its config would silently keep the old field set", want)
			}
		}
		for _, unwanted := range []string{"veh_rp_tomb", "veh_rp_drv", "veh_rp_novin"} {
			if _, ok := got[unwanted]; ok {
				t.Errorf("%s must NOT be listed — pushing at it is not the operator's to choose", unwanted)
			}
		}
		if len(rows) != 8 {
			t.Fatalf("len = %d, want exactly 8: %v", len(rows), repushIDs(rows))
		}
	})

	t.Run("carries the facts the sweep refuses on", func(t *testing.T) {
		rows, err := repo.ListStreamingFleetConfigVehicles(ctx, 100)
		if err != nil {
			t.Fatalf("ListStreamingFleetConfigVehicles: %v", err)
		}
		byID := map[string]store.StreamingFleetVehicle{}
		for _, r := range rows {
			byID[r.VehicleID] = r
		}

		if !byID["veh_rp_susp"].Suspended {
			t.Error("veh_rp_susp: Suspended = false — a re-push would reverse a cost decision")
		}
		if byID["veh_rp_warned"].Suspended {
			t.Error("veh_rp_warned: Suspended = true — a warned car is still streaming")
		}
		if !byID["veh_rp_absent"].ConfigAbsent {
			t.Error("veh_rp_absent: ConfigAbsent = false — awaiting_virtual_key means no config exists")
		}
		if byID["veh_rp_synced"].ConfigAbsent {
			t.Error("veh_rp_synced: ConfigAbsent = true — that outcome does not mean the config is missing")
		}
		if byID["veh_rp_live"].Suspended || byID["veh_rp_live"].ConfigAbsent {
			t.Errorf("veh_rp_live carries a refusal flag it should not: %+v", byID["veh_rp_live"])
		}
		for id, r := range byID {
			if r.PendingOwnerAck {
				t.Errorf("%s: PendingOwnerAck true — the anti-join should have removed it", id)
			}
			if len(r.VIN) != 17 {
				t.Errorf("%s: VIN = %q, want 17 chars", id, r.VIN)
			}
			if r.UserID == "" {
				t.Errorf("%s: UserID empty — the owner token cannot be resolved", id)
			}
			if r.LastUpdated.IsZero() {
				t.Errorf("%s: LastUpdated zero — the operator listing loses its staleness column", id)
			}
		}
	})

	t.Run("most recently active first so a cap reaches the live fleet", func(t *testing.T) {
		rows, err := repo.ListStreamingFleetConfigVehicles(ctx, 1)
		if err != nil {
			t.Fatalf("ListStreamingFleetConfigVehicles: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("len = %d, want 1 — the cap bounds one run", len(rows))
		}
		if rows[0].VehicleID != "veh_rp_live" {
			t.Errorf("first row = %s, want veh_rp_live (newest lastUpdated first)", rows[0].VehicleID)
		}
	})

	t.Run("non-positive limit refuses to scan", func(t *testing.T) {
		for _, limit := range []int{0, -1} {
			rows, err := repo.ListStreamingFleetConfigVehicles(ctx, limit)
			if err != nil {
				t.Fatalf("ListStreamingFleetConfigVehicles(limit=%d): %v", limit, err)
			}
			if len(rows) != 0 {
				t.Errorf("limit=%d returned %d rows, want 0", limit, len(rows))
			}
		}
	})
}

// H: "Vehicle"."teslaVehicleId" is NULLABLE and a NULL makes the tombstone
// equality NULL, which makes NOT EXISTS true and ADMITS the row. The candidate
// query next door has this test for the same predicate; this one exists because
// a copied predicate is exactly the kind that gets copied slightly wrong.
func TestStreamingFleetVehicles_TombstoneMatchesOnVINWhenTeslaIDIsNull(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	mustApplyOwnerSchema(t)
	cleanTables(t, testPool)
	cleanFleetCandidateTables(t)
	cleanRepushTables(t)

	ctx := context.Background()
	const owner = "user_rp_tomb_null"
	const vin = "7SAYGDED5TA736164"
	seedFCCAccount(t, "acct_rp_tomb_null", owner, "tesla-sub-rp-tomb")

	if _, err := testPool.Exec(ctx,
		`INSERT INTO "Vehicle" ("id", "userId", "vin", "teslaVehicleId", "name", "status", "lastUpdated")
		 VALUES ('veh_rp_tomb_null', $1, $2, NULL, 'Tesla', 'offline', NOW())`,
		owner, vin); err != nil {
		t.Fatalf("seed vehicle: %v", err)
	}
	seedFCCTombstone(t, owner, "tv_gone", vin)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	rows, err := repo.ListStreamingFleetConfigVehicles(ctx, 100)
	if err != nil {
		t.Fatalf("ListStreamingFleetConfigVehicles: %v", err)
	}
	for _, r := range rows {
		if r.VehicleID == "veh_rp_tomb_null" {
			t.Fatal("a tombstoned car with a NULL teslaVehicleId was admitted — " +
				"the sweep would push config at a deliberately removed vehicle")
		}
	}
}

// The "Account" semi-join the MYR-448 query carries is deliberately ABSENT
// here, but a user holding two tesla rows must still not appear twice: a
// duplicated row is a duplicated Tesla push and a duplicated slot in the cap.
func TestStreamingFleetVehicles_DuplicateTeslaAccountsDoNotFanOut(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	mustApplyOwnerSchema(t)
	cleanTables(t, testPool)
	cleanFleetCandidateTables(t)
	cleanRepushTables(t)

	ctx := context.Background()
	const owner = "user_rp_dupe"
	seedFCCAccount(t, "acct_rp_dupe_1", owner, "tesla-sub-rp-a")
	seedFCCAccount(t, "acct_rp_dupe_2", owner, "tesla-sub-rp-b")
	seedFleetCandidateVehicle(t, "veh_rp_dupe", owner, "7SAYGDED5TA736164", "tv_rp_dupe", time.Now())

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	rows, err := repo.ListStreamingFleetConfigVehicles(ctx, 100)
	if err != nil {
		t.Fatalf("ListStreamingFleetConfigVehicles: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len = %d, want 1 — two tesla Account rows must not duplicate the vehicle", len(rows))
	}
}

func repushIDs(rows []store.StreamingFleetVehicle) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.VehicleID)
	}
	return out
}

func cleanRepushTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM go_vehicle_telemetry_suspensions`,
		`DELETE FROM go_fleet_config_attempts`,
	} {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("clean (%s): %v", stmt, err)
		}
	}
}

func seedRepushSuspension(t *testing.T, vehicleID string, suspendedAt time.Time) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_vehicle_telemetry_suspensions (vehicle_id, warned_at, suspended_at)
		 VALUES ($1, $2, $3)`,
		vehicleID, suspendedAt.Add(-24*time.Hour), suspendedAt)
	if err != nil {
		t.Fatalf("seedRepushSuspension(%s): %v", vehicleID, err)
	}
}

func seedRepushWarning(t *testing.T, vehicleID string, warnedAt time.Time) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_vehicle_telemetry_suspensions (vehicle_id, warned_at) VALUES ($1, $2)`,
		vehicleID, warnedAt)
	if err != nil {
		t.Fatalf("seedRepushWarning(%s): %v", vehicleID, err)
	}
}

func seedRepushAttempt(t *testing.T, vehicleID, outcome string) {
	t.Helper()
	now := time.Now()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_fleet_config_attempts
		     (vehicle_id, attempt_count, last_attempt_at, next_attempt_at, last_outcome)
		 VALUES ($1, 1, $2, $3, $4)`,
		vehicleID, now, now.Add(time.Hour), outcome)
	if err != nil {
		t.Fatalf("seedRepushAttempt(%s): %v", vehicleID, err)
	}
}

func seedRepushDriverAccess(t *testing.T, vehicleID, userID string, acknowledged bool) {
	t.Helper()
	var ackAt any
	var version any
	if acknowledged {
		ackAt = time.Now()
		version = "owner-approval-v1"
	}
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_vehicle_driver_access
		     (vehicle_id, user_id, tesla_access_type, acknowledged_at, acknowledgment_version)
		 VALUES ($1, $2, 'DRIVER', $3, $4)`,
		vehicleID, userID, ackAt, version)
	if err != nil {
		t.Fatalf("seedRepushDriverAccess(%s): %v", vehicleID, err)
	}
}
