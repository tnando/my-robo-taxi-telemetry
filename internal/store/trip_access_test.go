package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// The ACCESS-SHAPED trips tests: the live-share join, the drives window, the
// catalog leg, and the mutations that end a window.
//
// These are the ones that would be silent if they were wrong. A broken create
// throws an error somebody sees; a broken access predicate hands a stranger a
// live map, or takes one away from a person mid-drive, and nothing complains.

// TestTripAccessCannotOutliveTheShare is the headline security assertion.
//
// The rule is that a trip decides only what an EXISTING share means between two
// instants, so revoking the share must end the trip access immediately —
// structurally, through the join every access query carries, and not through a
// cleanup job that could be skipped, delayed, or removed.
func TestTripAccessCannotOutliveTheShare(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	shareRepo := newShareRepo(t)
	now := time.Now().UTC()

	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareID})

	t.Run("while the share is live the window admits them", func(t *testing.T) {
		ids, err := repo.ActiveTripVehicleIDs(ctx, shareViewer1)
		if err != nil {
			t.Fatalf("ActiveTripVehicleIDs: %v", err)
		}
		if ids[vehicleID] != trip.ID {
			t.Fatalf("active trips = %v, want %s on %s", ids, trip.ID, vehicleID)
		}
		windows, err := repo.TripDriveWindows(ctx, shareViewer1, vehicleID)
		if err != nil {
			t.Fatalf("TripDriveWindows: %v", err)
		}
		if len(windows) != 1 {
			t.Fatalf("windows = %v, want one", windows)
		}
	})

	t.Run("revoking the share ends it, with no cascade required", func(t *testing.T) {
		if _, err := shareRepo.RevokeInvite(ctx, shareID, shareOwnerA); err != nil {
			t.Fatalf("RevokeInvite: %v", err)
		}

		// NOTE WHAT IS *NOT* CALLED HERE: RemoveParticipantsForShare. The
		// go_trip_participants row is still present and still has left_at NULL
		// — and the access is gone anyway, because every access query re-joins
		// the live grant. That is the property this test exists to pin, and it
		// is why the cascade can be described honestly as a roster repair.
		ids, err := repo.ActiveTripVehicleIDs(ctx, shareViewer1)
		if err != nil {
			t.Fatalf("ActiveTripVehicleIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("a revoked share still admits the vehicle: %v", ids)
		}
		windows, err := repo.TripDriveWindows(ctx, shareViewer1, vehicleID)
		if err != nil {
			t.Fatalf("TripDriveWindows: %v", err)
		}
		if len(windows) != 0 {
			t.Fatalf("a revoked share still admits the drives: %v", windows)
		}
	})

	t.Run("the cascade then repairs the roster", func(t *testing.T) {
		// The DISPLAY half. Without it the owner's card keeps listing somebody
		// who can no longer see anything, and the participant count lies.
		n, err := repo.RemoveParticipantsForShare(ctx, vehicleID, shareViewer1)
		if err != nil {
			t.Fatalf("RemoveParticipantsForShare: %v", err)
		}
		if n != 1 {
			t.Fatalf("cascade removed %d rows, want 1", n)
		}
		view, err := repo.GetForUser(ctx, trip.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("GetForUser: %v", err)
		}
		if len(view.Participants) != 0 {
			t.Fatalf("roster still lists %+v", view.Participants)
		}
	})
}

// TestTripWindowClosesAccessWithNothingToRevoke pins the other expiry: a window
// ends when the CLOCK passes an instant, and nothing writes a row at that
// moment.
func TestTripWindowClosesAccessWithNothingToRevoke(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	now := time.Now().UTC()

	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-2*time.Hour), now.Add(24*time.Hour), []string{shareID})

	ids, err := repo.ActiveTripVehicleIDs(ctx, shareViewer1)
	if err != nil {
		t.Fatalf("ActiveTripVehicleIDs: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("open window did not admit the vehicle: %v", ids)
	}

	// The OWNER ends it early. `ended_at` is stamped and `ends_at` is left
	// alone, so the owner's stated intent survives and an accidental early end
	// stays explainable.
	ended, err := repo.End(ctx, trip.ID, shareOwnerA)
	if err != nil {
		t.Fatalf("End: %v", err)
	}
	if ended.EndedAt == nil {
		t.Fatal("End did not stamp ended_at")
	}
	if !ended.EndsAt.Equal(trip.EndsAt) {
		t.Errorf("End overwrote the owner's stated window: %v, want %v", ended.EndsAt, trip.EndsAt)
	}
	if got := ended.StatusAt(time.Now()); got != store.TripStatusEnded {
		t.Errorf("status = %q, want ended", got)
	}

	ids, err = repo.ActiveTripVehicleIDs(ctx, shareViewer1)
	if err != nil {
		t.Fatalf("ActiveTripVehicleIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("a closed window still admits the vehicle: %v", ids)
	}

	t.Run("the drives survive the window", func(t *testing.T) {
		// ACTIVE **OR ENDED**, deliberately wider than the live-access
		// predicate: the window's drives are the record of a journey the person
		// was part of, and having the list go dark the moment the trip ends
		// would delete the feature exactly when it becomes worth reading.
		windows, err := repo.TripDriveWindows(ctx, shareViewer1, vehicleID)
		if err != nil {
			t.Fatalf("TripDriveWindows: %v", err)
		}
		if len(windows) != 1 {
			t.Fatalf("windows = %v, want the ended one to still admit its drives", windows)
		}
	})

	t.Run("End is idempotent and does not move the end forward", func(t *testing.T) {
		// A double-tap that re-stamped would silently extend somebody's live
		// location by however long the two taps were apart.
		again, err := repo.End(ctx, trip.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("second End: %v", err)
		}
		if !again.EndedAt.Equal(*ended.EndedAt) {
			t.Fatalf("ended_at moved from %v to %v on the second call", *ended.EndedAt, *again.EndedAt)
		}
	})
}

// TestScheduledWindowsAdmitNoDrives is a narrow but load-bearing assertion.
//
// If a scheduled window admitted its drives, an owner could grant read access
// to the PAST by scheduling a trip for next week over a range of past dates —
// the window has not opened, so nothing else would stop them.
func TestScheduledWindowsAdmitNoDrives(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	now := time.Now().UTC()

	mustCreateTrip(t, repo, vehicleID, now.Add(48*time.Hour), now.Add(96*time.Hour), []string{shareID})

	windows, err := repo.TripDriveWindows(ctx, shareViewer1, vehicleID)
	if err != nil {
		t.Fatalf("TripDriveWindows: %v", err)
	}
	if len(windows) != 0 {
		t.Fatalf("a scheduled window admitted drives: %v", windows)
	}
	ids, err := repo.ActiveTripVehicleIDs(ctx, shareViewer1)
	if err != nil {
		t.Fatalf("ActiveTripVehicleIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("a scheduled window admitted live access: %v", ids)
	}
}

// TestTripDrivesAreBoundedByTheWindow covers the drive list and the
// single-drive gate over the same data, INCLUDING both inclusive edges.
func TestTripDrivesAreBoundedByTheWindow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)

	// A window with instants chosen so each drive below sits unambiguously on
	// one side of an edge, or exactly on it.
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	trip := mustCreateTrip(t, repo, vehicleID, start, end, []string{shareID})

	drives := []struct {
		id string
		at time.Time
	}{
		{"cdrv_before", start.Add(-time.Hour)},
		{"cdrv_on_start", start},
		{"cdrv_inside", start.Add(24 * time.Hour)},
		{"cdrv_on_end", end},
		{"cdrv_after", end.Add(time.Hour)},
	}
	for _, d := range drives {
		seedDriveAt(t, d.id, vehicleID, d.at)
	}

	page, err := repo.TripDrivesForUser(ctx, trip.ID, shareViewer1, store.DriveListCursor{}, 10)
	if err != nil {
		t.Fatalf("TripDrivesForUser: %v", err)
	}
	got := map[string]bool{}
	for _, item := range page.Items {
		got[item.ID] = true
	}

	// BOTH EDGES ARE INCLUSIVE, which is deliberately asymmetric with the
	// access predicate's exclusive upper bound: access is about a live socket
	// at an instant, whereas a drive that began exactly at the closing instant
	// is a drive of this trip and excluding it would lose it from the only list
	// it belongs to.
	for _, want := range []string{"cdrv_on_start", "cdrv_inside", "cdrv_on_end"} {
		if !got[want] {
			t.Errorf("%s is inside the window but was not listed; got %v", want, got)
		}
	}
	for _, unwanted := range []string{"cdrv_before", "cdrv_after"} {
		if got[unwanted] {
			t.Errorf("%s is outside the window but was listed", unwanted)
		}
	}

	t.Run("CoversDrive agrees with the list, drive for drive", func(t *testing.T) {
		// The two MUST agree: a participant who can see a drive in the list and
		// gets 404 opening it is a bug, and so — in the direction that matters
		// — is the reverse. CoversDrive folds over the SAME windows the list
		// query uses, so the agreement is structural; this pins that it holds.
		for _, d := range drives {
			covered, err := repo.CoversDrive(ctx, shareViewer1, vehicleID, d.at)
			if err != nil {
				t.Fatalf("CoversDrive(%s): %v", d.id, err)
			}
			if covered != got[d.id] {
				t.Errorf("%s: CoversDrive = %v but the list says %v", d.id, covered, got[d.id])
			}
		}
	})

	t.Run("a stranger is admitted to nothing", func(t *testing.T) {
		windows, err := repo.TripDriveWindows(ctx, shareViewer2, vehicleID)
		if err != nil {
			t.Fatalf("TripDriveWindows: %v", err)
		}
		if len(windows) != 0 {
			t.Fatalf("a stranger got windows: %v", windows)
		}
		// AND the empty window set must return an EMPTY PAGE, not the whole
		// history. That is structural — unnest over two empty arrays matches
		// no row — and this is the assertion that keeps it so.
		page, err := repo.VehicleDrivesInTripWindows(ctx, shareViewer2, vehicleID, store.DriveListCursor{}, 10)
		if err != nil {
			t.Fatalf("VehicleDrivesInTripWindows: %v", err)
		}
		if len(page.Items) != 0 {
			t.Fatalf("a caller with no windows was served %d drives", len(page.Items))
		}
	})

	t.Run("the vehicle-scoped list is narrowed to the same set", func(t *testing.T) {
		page, err := repo.VehicleDrivesInTripWindows(ctx, shareViewer1, vehicleID, store.DriveListCursor{}, 10)
		if err != nil {
			t.Fatalf("VehicleDrivesInTripWindows: %v", err)
		}
		if len(page.Items) != 3 {
			t.Fatalf("§7.2 served %d drives to a participant, want the window's 3", len(page.Items))
		}
	})

	t.Run("driveCount counts the same window", func(t *testing.T) {
		view, err := repo.GetForUser(ctx, trip.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("GetForUser: %v", err)
		}
		if view.DriveCount != 3 {
			t.Errorf("driveCount = %d, want 3 — the card's number and the list must agree", view.DriveCount)
		}
	})
}

// TestPatchRefusesOnAnEndedTrip pins the rule that stops a lapsed window being
// resurrected.
//
// Extending `endsAt` past NOW() on an ended trip would hand live location back
// to people who were already told the trip was over. Continuing a road trip is
// a NEW trip, which says the true thing on everybody's phone.
func TestPatchRefusesOnAnEndedTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	now := time.Now().UTC()

	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-2*time.Hour), now.Add(time.Hour), []string{shareID})
	if _, err := repo.End(ctx, trip.ID, shareOwnerA); err != nil {
		t.Fatalf("End: %v", err)
	}

	extended := now.Add(72 * time.Hour)
	_, err := repo.Update(ctx, trip.ID, shareOwnerA, store.UpdateTripInput{EndsAt: &extended})
	if !errors.Is(err, store.ErrTripEnded) {
		t.Fatalf("err = %v, want ErrTripEnded", err)
	}

	t.Run("and a non-owner gets 404, not a denial", func(t *testing.T) {
		name := "renamed"
		_, err := repo.Update(ctx, trip.ID, shareViewer1, store.UpdateTripInput{Name: &name})
		if !errors.Is(err, store.ErrTripNotFound) {
			t.Fatalf("participant patching got %v, want ErrTripNotFound", err)
		}
	})
}

// TestPatchRefusesABackwardsEndsAt keeps `POST /end` and a shortened window
// from becoming the same button.
//
// Ending early stamps `ended_at` and leaves the owner's stated window intact,
// so an accidental early end stays explainable. A backwards `endsAt` would end
// the trip retroactively AND overwrite the intent, losing both.
func TestPatchRefusesABackwardsEndsAt(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	now := time.Now().UTC()

	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-24*time.Hour), now.Add(24*time.Hour), []string{shareID})

	past := now.Add(-time.Hour)
	if _, err := repo.Update(ctx, trip.ID, shareOwnerA, store.UpdateTripInput{EndsAt: &past}); !errors.Is(err, store.ErrTripWindowInvalid) {
		t.Fatalf("err = %v, want ErrTripWindowInvalid", err)
	}

	t.Run("shortening to a future instant is allowed", func(t *testing.T) {
		soon := now.Add(2 * time.Hour)
		view, err := repo.Update(ctx, trip.ID, shareOwnerA, store.UpdateTripInput{EndsAt: &soon})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if !view.EndsAt.Truncate(time.Second).Equal(soon.Truncate(time.Second)) {
			t.Errorf("endsAt = %v, want %v", view.EndsAt, soon)
		}
	})

	t.Run("extending past the 30-day cap is refused", func(t *testing.T) {
		// The cap is measured from startsAt, so a patch that only checked its
		// own delta could walk a trip past it one legal-looking step at a time.
		far := trip.StartsAt.Add(31 * 24 * time.Hour)
		if _, err := repo.Update(ctx, trip.ID, shareOwnerA, store.UpdateTripInput{EndsAt: &far}); !errors.Is(err, store.ErrTripWindowInvalid) {
			t.Fatalf("err = %v, want ErrTripWindowInvalid", err)
		}
	})
}

// seedDriveAt installs one Drive row starting at a given instant.
//
// `startTime` is a TEXT column holding RFC 3339 (a Prisma-owned shape), which
// is exactly why the window query casts it to timestamptz rather than comparing
// strings — see queryTripDrivesWindow.
func seedDriveAt(t *testing.T, driveID, vehicleID string, at time.Time) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Drive" ("id","vehicleId","date","startTime") VALUES ($1,$2,$3,$4)`,
		driveID, vehicleID, at.Format("2006-01-02"), at.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
}
