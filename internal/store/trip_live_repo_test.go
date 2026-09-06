package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// The LIVE trip repositories against a real Postgres (MYR-602).
//
// What is asserted here is what only a real database can answer: that the
// CLAIMS are atomic, that the one-open-leg-per-trip partial index actually
// refuses a second leg, and that the two encrypted columns round-trip. Every
// one of those is a property of a statement or an index, not of Go code, so a
// fake would assert the test's own opinion rather than the schema's behaviour.

func seedTripWindow(t *testing.T, id, vehicleID, ownerID string, startsAt, endsAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_trips (id, vehicle_id, owner_user_id, name_enc, starts_at, ends_at)
		 VALUES ($1, $2, $3, 'seeded', $4, $5)
		 ON CONFLICT (id) DO UPDATE SET starts_at = $4, ends_at = $5,
		   started_notified_at = NULL, ended_notified_at = NULL, ended_at = NULL`,
		id, vehicleID, ownerID, startsAt, endsAt); err != nil {
		t.Fatalf("seed trip %s: %v", id, err)
	}
}

func newTripLiveRepo(t *testing.T) *store.TripLiveRepo {
	t.Helper()
	repo, err := store.NewTripLiveRepo(testPool, newTestEncryptor(t), nil, nil)
	if err != nil {
		t.Fatalf("NewTripLiveRepo: %v", err)
	}
	return repo
}

func newTripLegRepo(t *testing.T) *store.TripLegRepo {
	t.Helper()
	repo, err := store.NewTripLegRepo(testPool, newTestEncryptor(t), nil, nil)
	if err != nil {
		t.Fatalf("NewTripLegRepo: %v", err)
	}
	return repo
}

// TestTripClaims_AreOneShot is the property the whole sweeper rests on: each
// edge is claimed by exactly one caller, whatever races it.
func TestTripClaims_AreOneShot(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := newTripLiveRepo(t)

	now := time.Now().UTC()
	seedTripWindow(t, "ctrip0602open", "cveh0602", "cowner0602",
		now.Add(-time.Hour), now.Add(time.Hour))

	first, err := repo.ClaimTripsToStart(ctx, 50)
	if err != nil {
		t.Fatalf("ClaimTripsToStart: %v", err)
	}
	if !containsString(first, "ctrip0602open") {
		t.Fatalf("the open trip was not claimed: %v", first)
	}
	second, err := repo.ClaimTripsToStart(ctx, 50)
	if err != nil {
		t.Fatalf("ClaimTripsToStart (second): %v", err)
	}
	if containsString(second, "ctrip0602open") {
		t.Error("the same trip was claimed twice; the stamp is not arbitrating and every " +
			"participant would be told the trip started once a minute forever")
	}

	// The single-trip claim shares the stamp, which is what makes a
	// create-with-a-past-start racing the sweeper announce exactly once.
	claimed, err := repo.ClaimTripStartNow(ctx, "ctrip0602open")
	if err != nil {
		t.Fatalf("ClaimTripStartNow: %v", err)
	}
	if claimed {
		t.Error("ClaimTripStartNow won a stamp the sweeper had already taken")
	}
}

// TestClaimTripStartNow_RefusesAnUnopenedWindow. The end claim can be taken on
// the strength of a column the caller just wrote; a START has no such write
// behind it, so announcing one for a window that has not begun is the one
// direction the stamp can never take back.
func TestClaimTripStartNow_RefusesAnUnopenedWindow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := newTripLiveRepo(t)

	now := time.Now().UTC()
	seedTripWindow(t, "ctrip0602future", "cveh0602f", "cowner0602",
		now.Add(24*time.Hour), now.Add(48*time.Hour))

	claimed, err := repo.ClaimTripStartNow(ctx, "ctrip0602future")
	if err != nil {
		t.Fatalf("ClaimTripStartNow: %v", err)
	}
	if claimed {
		t.Error("a trip whose window has not opened was announced as started")
	}
}

// TestTripAudienceFor_ExcludesDepartedAndSuspended proves the two access
// predicates on the fan-out read. A notification naming somebody's car IS a
// surface, and a suspended grantee must be indistinguishable from no grantee on
// every one of them.
func TestTripAudienceFor_ExcludesDepartedAndSuspended(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := newTripLiveRepo(t)

	const tripID, vehicleID = "ctrip0602aud", "cveh0602aud"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, "cowner0602aud", now.Add(-time.Hour), now.Add(time.Hour))

	type participant struct {
		userID    string
		left      bool
		suspended bool
		revoked   bool
	}
	people := []participant{
		{userID: "cp0602live"},
		{userID: "cp0602left", left: true},
		{userID: "cp0602susp", suspended: true},
		{userID: "cp0602revk", revoked: true},
	}
	for _, p := range people {
		status := "accepted"
		if p.revoked {
			status = "revoked"
		}
		var suspendedAt any
		if p.suspended {
			suspendedAt = now
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_vehicle_shares (id, vehicle_id, owner_user_id, label, permission,
			   code, status, expires_at, accepted_by_user_id, suspended_at)
			 VALUES ('sh_' || $1, $2, 'cowner0602aud', 'Seeded', 'live',
			         'code_' || $1, $3, NOW() + INTERVAL '30 days', $1, $4)
			 ON CONFLICT (id) DO UPDATE SET status = $3, suspended_at = $4`,
			p.userID, vehicleID, status, suspendedAt); err != nil {
			t.Fatalf("seed share for %s: %v", p.userID, err)
		}
		var leftAt any
		if p.left {
			leftAt = now
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_trip_participants (trip_id, user_id, share_id, left_at)
			 VALUES ($1, $2, 'sh_' || $2, $3)
			 ON CONFLICT (trip_id, user_id) DO UPDATE SET left_at = $3`,
			tripID, p.userID, leftAt); err != nil {
			t.Fatalf("seed participant %s: %v", p.userID, err)
		}
	}

	got, err := repo.TripAudienceFor(ctx, tripID)
	if err != nil {
		t.Fatalf("TripAudienceFor: %v", err)
	}
	if got.VehicleID != vehicleID || got.OwnerUserID != "cowner0602aud" {
		t.Errorf("audience = %+v, want the seeded vehicle and owner", got)
	}
	if len(got.ParticipantUserIDs) != 1 || got.ParticipantUserIDs[0] != "cp0602live" {
		t.Errorf("participants = %v, want only cp0602live — departed, suspended and "+
			"revoked must every one be indistinguishable from absent",
			got.ParticipantUserIDs)
	}
}

// TestStartLeg_OneOpenLegPerTrip proves the partial unique index refuses a
// second open leg, and that the repo turns that refusal into the EXISTING leg
// rather than an error. One duplicate leg is one duplicate Live Activity on
// every participant's lock screen for the same journey.
func TestStartLeg_OneOpenLegPerTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID = "ctrip0602leg", "cveh0602leg"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, "cowner0602leg", now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := testPool.Exec(ctx, `DELETE FROM go_trip_legs WHERE trip_id = $1`, tripID); err != nil {
		t.Fatalf("clean legs: %v", err)
	}

	repo := newTripLegRepo(t)
	first, err := repo.StartLeg(ctx, tripID, vehicleID, "Grand Canyon", now)
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}
	if first.ID == "" {
		t.Fatal("StartLeg returned no leg")
	}
	// P1 ROUND TRIP: the destination is sealed on the way in and opened on the
	// way out, through the same MYR-447 label encryptor the vehicle repo uses.
	if first.DestinationName != "Grand Canyon" {
		t.Errorf("destination = %q, want Grand Canyon — the sealed column did not round-trip",
			first.DestinationName)
	}

	second, err := repo.StartLeg(ctx, tripID, vehicleID, "Sedona", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second StartLeg: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("a second leg %q was opened while %q was still open", second.ID, first.ID)
	}

	// Once it is closed, a new leg may open.
	if err := repo.EndLeg(ctx, first.ID, now.Add(time.Hour), true); err != nil {
		t.Fatalf("EndLeg: %v", err)
	}
	third, err := repo.StartLeg(ctx, tripID, vehicleID, "Sedona", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("third StartLeg: %v", err)
	}
	if third.ID == first.ID {
		t.Error("the closed leg was returned instead of a new one")
	}
}

// TestLegClaims_AreOneShot: four independent deliveries, four independent
// stamps. An alert can succeed while a push-to-start fails, and each must be
// retryable without re-sending the other.
func TestLegClaims_AreOneShot(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID = "ctrip0602claims", "cveh0602claims"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, "cowner0602claims", now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := testPool.Exec(ctx, `DELETE FROM go_trip_legs WHERE trip_id = $1`, tripID); err != nil {
		t.Fatalf("clean legs: %v", err)
	}

	repo := newTripLegRepo(t)
	leg, err := repo.StartLeg(ctx, tripID, vehicleID, "Somewhere", now)
	if err != nil {
		t.Fatalf("StartLeg: %v", err)
	}

	claims := map[string]func(context.Context, string) (bool, error){
		"started_push":   repo.ClaimLegStartedPush,
		"arrived_push":   repo.ClaimLegArrivedPush,
		"activity_start": repo.ClaimLegActivityStart,
		"activity_end":   repo.ClaimLegActivityEnd,
	}
	for name, claim := range claims {
		t.Run(name, func(t *testing.T) {
			first, err := claim(ctx, leg.ID)
			if err != nil {
				t.Fatalf("first claim: %v", err)
			}
			if !first {
				t.Fatal("the first claim lost")
			}
			second, err := claim(ctx, leg.ID)
			if err != nil {
				t.Fatalf("second claim: %v", err)
			}
			if second {
				t.Error("the second claim also won; this delivery would repeat forever")
			}
		})
	}
}

// TestPushToStartTokens_UpsertAndReject covers the rotation (ActivityKit
// rotates the push-to-start token, so a re-registration must REPLACE rather
// than accumulate) and the 410 path that deletes by value.
func TestPushToStartTokens_UpsertAndReject(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID = "ctrip0602tok"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, "cveh0602tok", "cowner0602tok", now.Add(-time.Hour), now.Add(time.Hour))

	repo := store.NewTripActivityTokenRepo(testPool, nil)
	if err := repo.RegisterPushToStartToken(ctx, tripID, "cuser0602tok", "pts-v1", true); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := repo.RegisterPushToStartToken(ctx, tripID, "cuser0602tok", "pts-v2", false); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	tokens, err := repo.PushToStartTokensForTrip(ctx, tripID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("%d rows after a rotation, want 1 — keying on the token would accumulate "+
			"one row per rotation and leave the sender guessing which is live", len(tokens))
	}
	if tokens[0].PushToStartToken != "pts-v2" || tokens[0].Sandbox {
		t.Errorf("row = %+v, want the rotated token and its sandbox flag", tokens[0])
	}

	if err := repo.DeleteRejectedPushToStartToken(ctx, "pts-v2"); err != nil {
		t.Fatalf("delete rejected: %v", err)
	}
	tokens, err = repo.PushToStartTokensForTrip(ctx, tripID)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("%d rows survived the 410; the app is gone and the token addresses nothing",
			len(tokens))
	}
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}
