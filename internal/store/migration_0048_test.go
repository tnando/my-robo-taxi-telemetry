package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-602 — migration 0048: the sixth notification category.
//
// One column, and three things about it are load-bearing enough that a silent
// change to any of them would be the MYR-349 lie restored rather than a
// slowdown:
//
//  1. NOT NULL. A NULL preference has no meaning: the gate reads a boolean, and
//     a scan into a plain bool would fail rather than fall back.
//  2. DEFAULT TRUE. Every account that existed before this migration must be
//     receiving trip notifications afterwards, and a person with NO ROW AT ALL
//     resolves to DefaultPushPrefs, which is also all-on. The two paths agree
//     by construction rather than by care.
//  3. THE ROUND TRIP. Reading back what was written is the only assertion that
//     covers the SEVEN-parameter upsert's positional argument list, where a
//     mis-numbered `$7` would silently write the trips answer into
//     viewer_joined and vice versa. Both are booleans, so nothing else would
//     catch it.

func TestMigration0048_TripsPrefColumn(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	var isNullable, columnDefault string
	err := testPool.QueryRow(ctx,
		`SELECT is_nullable, COALESCE(column_default, '')
		   FROM information_schema.columns
		  WHERE table_name = 'go_push_prefs' AND column_name = 'trips'`).
		Scan(&isNullable, &columnDefault)
	if err != nil {
		t.Fatalf("read the trips column: %v (missing after migrate up?)", err)
	}
	if isNullable != "NO" {
		t.Errorf("trips is_nullable = %q, want NOT NULL — a NULL preference has no meaning",
			isNullable)
	}
	if columnDefault != "true" {
		t.Errorf("trips column_default = %q, want true — every account that existed before "+
			"this migration must keep receiving trip notifications", columnDefault)
	}
}

// TestMigration0048_ExistingRowsDefaultOn covers the backfill from the other
// side: a row written BEFORE the column existed reads back as opted in.
func TestMigration0048_ExistingRowsDefaultOn(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const userID = "cuser0048legacy"
	if _, err := testPool.Exec(ctx,
		`DELETE FROM go_push_prefs WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("clean: %v", err)
	}
	// Written naming only the five pre-0048 columns, exactly as a row inserted
	// before this migration would look.
	if _, err := testPool.Exec(ctx, `
		INSERT INTO go_push_prefs
		  (user_id, ride_lifecycle, drive_started, drive_completed, charging_complete, viewer_joined)
		VALUES ($1, TRUE, FALSE, TRUE, FALSE, TRUE)`, userID); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	repo := store.NewPushPrefsRepo(testPool, nil)
	got, err := repo.PrefsForUser(ctx, userID)
	if err != nil {
		t.Fatalf("PrefsForUser: %v", err)
	}
	if !got.Trips {
		t.Error("a pre-0048 row reads back with trips OFF; the DEFAULT did not backfill")
	}
	// The five it was written with must be unchanged: a migration that flipped
	// a neighbour would be the same class of defect in the other direction.
	if !got.RideLifecycle || got.DriveStarted || !got.DriveCompleted ||
		got.ChargingComplete || !got.ViewerJoined {
		t.Errorf("the pre-existing five moved: %+v", got)
	}
}

// TestPushPrefs_TripsRoundTrip covers the upsert's positional arguments. Every
// column here is a boolean, so a mis-numbered parameter writes the right VALUE
// into the wrong SWITCH and nothing else in the suite would notice.
func TestPushPrefs_TripsRoundTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const userID = "cuser0048roundtrip"
	if _, err := testPool.Exec(ctx,
		`DELETE FROM go_push_prefs WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("clean: %v", err)
	}
	repo := store.NewPushPrefsRepo(testPool, nil)

	off := false
	got, err := repo.UpdatePrefs(ctx, userID, store.PushPrefsUpdate{Trips: &off})
	if err != nil {
		t.Fatalf("UpdatePrefs: %v", err)
	}
	if got.Trips {
		t.Error("trips stayed on after being switched off")
	}
	// A PARTIAL update leaves every other category alone, which on a row that
	// did not exist means the all-on default.
	if !got.RideLifecycle || !got.DriveStarted || !got.DriveCompleted ||
		!got.ChargingComplete || !got.ViewerJoined {
		t.Errorf("switching trips off moved another category: %+v", got)
	}

	// And it survives a read.
	readBack, err := repo.PrefsForUser(ctx, userID)
	if err != nil {
		t.Fatalf("PrefsForUser: %v", err)
	}
	if readBack.Trips {
		t.Error("trips read back ON after being stored OFF")
	}

	// Switching a NEIGHBOUR must not disturb trips — the mis-numbered-parameter
	// case in the other direction.
	got, err = repo.UpdatePrefs(ctx, userID, store.PushPrefsUpdate{ViewerJoined: &off})
	if err != nil {
		t.Fatalf("UpdatePrefs(viewerJoined): %v", err)
	}
	if got.Trips {
		t.Error("trips came back ON when viewerJoined was switched off")
	}
	if got.ViewerJoined {
		t.Error("viewerJoined stayed on")
	}
}
