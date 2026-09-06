package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Integration tests for the MYR-602 trips repository, against a real Postgres.
//
// They exist for the rules that CANNOT be asserted against a fake: the window
// CHECK constraints, the overlap probe, the live-share join that makes trip
// access unable to outlive a grant, the drive window's inclusive bound, and the
// deletion cascade. Handler-level behaviour (statuses, sub-codes, what reaches
// the wire) is asserted in internal/telemetry with a hand-written store.

// newTripRepo builds the repository over the shared test pool.
func newTripRepo(t *testing.T) *store.TripRepo {
	t.Helper()
	return store.NewTripRepo(testPool, store.NoopMetrics{}, newTestEncryptor(t), testLogger())
}

// seedTripFixture installs the share world plus one ACCEPTED grant from owner A
// to viewer 1 on vehicle A1, and returns the vehicle and the share id.
//
// The accepted grant is the whole precondition of the feature: a participant is
// picked FROM a share, and every access predicate re-joins that share. A
// fixture that skipped it would test a state the product cannot reach.
func seedTripFixture(t *testing.T) (vehicleID, shareID string) {
	t.Helper()

	vehA1, _, _ := seedShareFixtures(t)
	cleanTrips(t)
	shareRepo := newShareRepo(t)
	invite := mustCreateInvite(t, shareRepo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)
	if _, err := shareRepo.RedeemCode(context.Background(), invite.Code, shareViewer1); err != nil {
		t.Fatalf("RedeemCode: %v", err)
	}
	return vehA1, invite.ID
}

// cleanTrips empties the four trip tables.
//
// NEEDED SEPARATELY from seedShareFixtures' own cleanup, because go_trips
// deliberately holds NO foreign key to the Prisma-owned "Vehicle" (CG-DL-9), so
// deleting the vehicles does NOT cascade to the trips on them. A suite that
// skipped this would carry one test's windows into the next and every overlap
// probe after the first would fail — which is exactly how this helper came to
// be written.
//
// One statement: the other three tables cascade off go_trips(id).
func cleanTrips(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM go_trips`); err != nil {
		t.Fatalf("clean go_trips: %v", err)
	}
}

// mustCreateTrip opens a window and fails the test if it does not.
func mustCreateTrip(t *testing.T, repo *store.TripRepo, vehicleID string, startsAt, endsAt time.Time, shareIDs []string) store.TripView {
	t.Helper()
	view, err := repo.Create(context.Background(), store.CreateTripInput{
		VehicleID:           vehicleID,
		OwnerUserID:         shareOwnerA,
		Name:                "DFW → LA",
		StartsAt:            startsAt,
		EndsAt:              endsAt,
		ParticipantShareIDs: shareIDs,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return view
}

// TestTripRepo_CreateAndRead covers the happy path and the two role
// resolutions, which are the same statement answering for two callers.
func TestTripRepo_CreateAndRead(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	created := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareID})

	t.Run("the name round-trips through the encryptor", func(t *testing.T) {
		// The column is ciphertext; if the seal and the open disagreed this
		// would come back as base64 rather than as the name.
		if created.Name != "DFW → LA" {
			t.Fatalf("name = %q, want the plaintext back", created.Name)
		}
	})

	t.Run("the owner reads it as owner", func(t *testing.T) {
		view, err := repo.GetForUser(ctx, created.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("GetForUser(owner): %v", err)
		}
		if view.Role != "owner" {
			t.Errorf("role = %q, want owner", view.Role)
		}
		if got := view.StatusAt(time.Now()); got != store.TripStatusActive {
			t.Errorf("status = %q, want active for a window that opened an hour ago", got)
		}
	})

	t.Run("the participant reads it as participant", func(t *testing.T) {
		view, err := repo.GetForUser(ctx, created.ID, shareViewer1)
		if err != nil {
			t.Fatalf("GetForUser(participant): %v", err)
		}
		if view.Role != "participant" {
			t.Errorf("role = %q, want participant", view.Role)
		}
		// EVERYONE SEES THE WHOLE ROSTER — they are on a trip together.
		if len(view.Participants) != 1 || view.Participants[0].ParticipantID != shareID {
			t.Errorf("roster = %+v, want the one share id", view.Participants)
		}
	})

	t.Run("a stranger gets ErrTripNotFound, never a denial", func(t *testing.T) {
		// The SAME error an unknown id produces. A trip somebody else owns must
		// be indistinguishable from one that does not exist, or the endpoint is
		// an oracle for trip ids.
		_, err := repo.GetForUser(ctx, created.ID, shareViewer2)
		if !errors.Is(err, store.ErrTripNotFound) {
			t.Fatalf("stranger got %v, want ErrTripNotFound", err)
		}
		_, err = repo.GetForUser(ctx, "ctrp_does_not_exist", shareOwnerA)
		if !errors.Is(err, store.ErrTripNotFound) {
			t.Fatalf("unknown id got %v, want ErrTripNotFound", err)
		}
	})
}

// TestTripRepo_WindowRules covers the two CHECK constraints from the Go side,
// which is where the API's 400 comes from.
func TestTripRepo_WindowRules(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, _ := seedTripFixture(t)
	repo := newTripRepo(t)
	now := time.Now().UTC()

	cases := []struct {
		name             string
		startsAt, endsAt time.Time
		wantErr          error
	}{
		{"a zero-length window is not a window", now, now, store.ErrTripWindowInvalid},
		{"an inverted window is refused", now.Add(time.Hour), now, store.ErrTripWindowInvalid},
		{"31 days exceeds the cap", now, now.Add(31 * 24 * time.Hour), store.ErrTripWindowInvalid},
		// THE CAP IS A CEILING ON A STANDING LIVE-LOCATION GRANT, so exactly 30
		// days must still be accepted — an off-by-one here would silently
		// refuse the longest legitimate road trip.
		{"exactly 30 days is accepted", now, now.Add(30 * 24 * time.Hour), nil},
		// A WINDOW MAY START IN THE PAST. That is how the legs of a road trip
		// already driven join the trip, and it is a stated product requirement
		// rather than an oversight to guard against.
		{"a window may start in the past", now.Add(-72 * time.Hour), now.Add(24 * time.Hour), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view, err := repo.Create(ctx, store.CreateTripInput{
				VehicleID:   vehicleID,
				OwnerUserID: shareOwnerA,
				Name:        "Window",
				StartsAt:    tc.startsAt,
				EndsAt:      tc.endsAt,
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			// Clean up so the next accepted case does not collide on overlap.
			if _, endErr := repo.End(ctx, view.ID, shareOwnerA); endErr != nil {
				t.Fatalf("End: %v", endErr)
			}
		})
	}
}

// TestTripRepo_OverlapProbe covers the 409, including the case that makes
// BACKFILL work.
func TestTripRepo_OverlapProbe(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, _ := seedTripFixture(t)
	repo := newTripRepo(t)
	now := time.Now().UTC()

	live := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(48*time.Hour), nil)

	t.Run("an intersecting window on the same car is refused", func(t *testing.T) {
		_, err := repo.Create(ctx, store.CreateTripInput{
			VehicleID: vehicleID, OwnerUserID: shareOwnerA, Name: "Second",
			StartsAt: now.Add(24 * time.Hour), EndsAt: now.Add(72 * time.Hour),
		})
		if !errors.Is(err, store.ErrTripOverlap) {
			t.Fatalf("err = %v, want ErrTripOverlap", err)
		}
	})

	t.Run("a disjoint window is fine", func(t *testing.T) {
		view, err := repo.Create(ctx, store.CreateTripInput{
			VehicleID: vehicleID, OwnerUserID: shareOwnerA, Name: "Later",
			StartsAt: now.Add(72 * time.Hour), EndsAt: now.Add(96 * time.Hour),
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := repo.End(ctx, view.ID, shareOwnerA); err != nil {
			t.Fatalf("End: %v", err)
		}
	})

	t.Run("an ENDED window stops reserving the calendar", func(t *testing.T) {
		// THIS IS THE BACKFILL CASE and the reason the probe carries its third
		// predicate. A trip may start in the past, so a new window will
		// routinely cover instants old FINISHED windows also covered; only
		// scheduled-or-active trips can conflict. History does not book the
		// calendar.
		if _, err := repo.End(ctx, live.ID, shareOwnerA); err != nil {
			t.Fatalf("End: %v", err)
		}
		view, err := repo.Create(ctx, store.CreateTripInput{
			VehicleID: vehicleID, OwnerUserID: shareOwnerA, Name: "Same window again",
			StartsAt: now.Add(-time.Hour), EndsAt: now.Add(48 * time.Hour),
		})
		if err != nil {
			t.Fatalf("Create over an ended window: %v", err)
		}
		if _, err := repo.End(ctx, view.ID, shareOwnerA); err != nil {
			t.Fatalf("End: %v", err)
		}
	})
}

// TestTripRepo_ParticipantsMustBeAcceptedShares covers the 400, in all four of
// the ways it can be reached.
func TestTripRepo_ParticipantsMustBeAcceptedShares(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, _, vehB := seedShareFixtures(t)
	cleanTrips(t)
	shareRepo := newShareRepo(t)
	repo := newTripRepo(t)
	now := time.Now().UTC()

	acceptedOnA := mustCreateInvite(t, shareRepo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)
	if _, err := shareRepo.RedeemCode(ctx, acceptedOnA.Code, shareViewer1); err != nil {
		t.Fatalf("RedeemCode: %v", err)
	}
	pendingOnA := mustCreateInvite(t, shareRepo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)
	acceptedOnB := mustCreateInvite(t, shareRepo, shareOwnerB, vehB, []string{vehB}, store.SharePermissionLive)
	if _, err := shareRepo.RedeemCode(ctx, acceptedOnB.Code, shareViewer2); err != nil {
		t.Fatalf("RedeemCode: %v", err)
	}

	cases := []struct {
		name     string
		shareIDs []string
	}{
		{"a share id that does not exist", []string{"csh_nope"}},
		{"an invite that was never redeemed", []string{pendingOnA.ID}},
		{"an accepted share on a DIFFERENT car", []string{acceptedOnB.ID}},
		{"one good id and one bad — ALL OR NOTHING", []string{acceptedOnA.ID, pendingOnA.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ONE ANSWER FOR ALL FOUR, deliberately: reporting which id failed
			// would make the endpoint an oracle for other people's share ids.
			_, err := repo.Create(ctx, store.CreateTripInput{
				VehicleID: vehA1, OwnerUserID: shareOwnerA, Name: "Trip",
				StartsAt: now, EndsAt: now.Add(24 * time.Hour),
				ParticipantShareIDs: tc.shareIDs,
			})
			if !errors.Is(err, store.ErrTripParticipantNotShared) {
				t.Fatalf("err = %v, want ErrTripParticipantNotShared", err)
			}
		})
	}

	t.Run("the all-or-nothing refusal wrote no trip", func(t *testing.T) {
		// A create that half-succeeded would leave a window the owner believes
		// has four people on it and that has three. One transaction, or none.
		views, err := repo.ListForUser(ctx, shareOwnerA, "", 0)
		if err != nil {
			t.Fatalf("ListForUser: %v", err)
		}
		if len(views) != 0 {
			t.Fatalf("found %d trips after four refused creates, want 0", len(views))
		}
	})
}
