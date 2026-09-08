package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// THE BANNER WINDOW, against the real schema (MYR-620).
//
// The rule is an UPSERT-as-claim with a predicate on the stored stamp: only a
// real database can say whether two racers on one flap resolve to one send, and
// whether a suppressed attempt leaves the stamp where it was.

// TestClaimLegBannerSlot_OnePerWindow.
func TestClaimLegBannerSlot_OnePerWindow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, vehicleID = "ctrip0620slot", "cveh0620slot"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, vehicleID, "cowner0620slot", now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := testPool.Exec(ctx, `DELETE FROM go_trip_leg_banners WHERE trip_id = $1`, tripID); err != nil {
		t.Fatalf("clean slots: %v", err)
	}

	repo := newTripLegRepo(t)
	const window = 30 * time.Minute
	const sedona, canyon = "digest-sedona", "digest-canyon"

	first, err := repo.ClaimLegBannerSlot(ctx, tripID, "trip_leg_started", sedona, now, window)
	if err != nil || !first {
		t.Fatalf("first claim = %v, %v; want true", first, err)
	}

	// The flap: ten more attempts inside the minute.
	for i := 1; i <= 10; i++ {
		at := now.Add(time.Duration(i) * 5 * time.Second)
		again, err := repo.ClaimLegBannerSlot(ctx, tripID, "trip_leg_started", sedona, at, window)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if again {
			t.Fatalf("attempt %d won the slot; this is the client's ten banners in an hour", i)
		}
	}

	// A DIFFERENT PLACE is different news, and a DIFFERENT EVENT reports the
	// outcome. Neither is suppressed.
	if ok, err := repo.ClaimLegBannerSlot(ctx, tripID, "trip_leg_started", canyon, now, window); err != nil || !ok {
		t.Errorf("a different destination was suppressed (%v, %v)", ok, err)
	}
	if ok, err := repo.ClaimLegBannerSlot(ctx, tripID, "trip_leg_arrived", sedona, now, window); err != nil || !ok {
		t.Errorf("the arrival was suppressed by the departure (%v, %v)", ok, err)
	}

	// THE STAMP IS ADVANCED ONLY BY A WINNER: ten refused attempts must not
	// have pushed the next legitimate banner half an hour further out. One
	// window after the FIRST send, the slot is free.
	after := now.Add(window + time.Second)
	if ok, err := repo.ClaimLegBannerSlot(ctx, tripID, "trip_leg_started", sedona, after, window); err != nil || !ok {
		t.Errorf("the window never reopened (%v, %v); the refused attempts moved the stamp", ok, err)
	}
}

// TestClaimLegBannerSlot_AnEmptyKeyIsAlwaysAllowed: a banner with no place in it
// says nothing that could be repeated, and one shared slot for every nameless
// leg would collapse genuinely different journeys.
func TestClaimLegBannerSlot_AnEmptyKeyIsAlwaysAllowed(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := newTripLegRepo(t)

	for i := 0; i < 3; i++ {
		ok, err := repo.ClaimLegBannerSlot(ctx, "ctrip0620empty", "trip_leg_started", "",
			time.Now().UTC(), 30*time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim %d = %v, %v; want true with no key", i, ok, err)
		}
	}
}

// TestHasPushToStartToken is the leg-banner gate's whole input: does a card
// reach this person's phone for this trip?
func TestHasPushToStartToken(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const tripID, ownerID = "ctrip0620pres", "cowner0620pres"
	now := time.Now().UTC()
	seedTripWindow(t, tripID, "cveh0620pres", ownerID, now.Add(-time.Hour), now.Add(time.Hour))

	repo := store.NewTripActivityTokenRepo(testPool, nil)
	if has, err := repo.HasPushToStartToken(ctx, tripID, ownerID); err != nil || has {
		t.Fatalf("HasPushToStartToken before registering = %v, %v; want false — a phone "+
			"with Live Activities off never registers and must keep its banner", has, err)
	}

	if err := newTripRepo(t).RegisterActivityStartToken(ctx, tripID, ownerID, "pts-0620", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	if has, err := repo.HasPushToStartToken(ctx, tripID, ownerID); err != nil || !has {
		t.Errorf("HasPushToStartToken after registering = %v, %v; want true", has, err)
	}

	// A 410 deletes the row, and its owner must go back to being told in prose.
	if err := repo.DeleteRejectedPushToStartToken(ctx, "pts-0620"); err != nil {
		t.Fatalf("delete rejected: %v", err)
	}
	if has, err := repo.HasPushToStartToken(ctx, tripID, ownerID); err != nil || has {
		t.Errorf("HasPushToStartToken after a 410 = %v, %v; want false — the app is gone "+
			"and that phone must not be left dark", has, err)
	}
}
