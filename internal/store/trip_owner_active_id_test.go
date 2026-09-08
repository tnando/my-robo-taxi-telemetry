package store_test

import (
	"context"
	"testing"
	"time"
)

// MYR-612 — THE OWNER'S OWN CAR CARRIES `activeTripId`.
//
// The catalog's trip leg answers two questions that look alike and are not:
//
//	ActiveTripVehicleIDs   which cars does a trip ADD to this catalog
//	ActiveTripIDsForUser   which of the cars already in it have a window open
//
// An owner's own car is in the second set and NEVER in the first — a trip adds
// no row for a car you already own. One statement served both, so the owner's
// row never carried the field. The iOS client registers its ActivityKit
// push-to-start token for the trips the catalog names, so on 2026-09-08 the
// OWNER of the car on the trip registered no token at all and could never have
// received a leg card on his own trip, whatever the leg detector did.
//
// The handler half was already right and already tested
// (TestActiveTripIDIsStampedOnTheOwnersOwnRow): it stamps whatever this map
// contains. The map was empty.

func TestActiveTripIDsForUserIncludesTheOwnersOwnCar(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	now := time.Now().UTC()

	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareID})

	t.Run("the owner", func(t *testing.T) {
		ids, err := repo.ActiveTripIDsForUser(ctx, shareOwnerA)
		if err != nil {
			t.Fatalf("ActiveTripIDsForUser: %v", err)
		}
		if ids[vehicleID] != trip.ID {
			t.Fatalf("the owner's own car carries no activeTripId: %v", ids)
		}
	})

	t.Run("the participant, unchanged", func(t *testing.T) {
		ids, err := repo.ActiveTripIDsForUser(ctx, shareViewer1)
		if err != nil {
			t.Fatalf("ActiveTripIDsForUser: %v", err)
		}
		if ids[vehicleID] != trip.ID {
			t.Fatalf("the participant lost their activeTripId: %v", ids)
		}
	})

	// THE MERGE LEG STAYS PARTICIPANT-ONLY. Widening that one instead would
	// have fed the owner's own car into the share-projection read, which
	// returns nothing for a car nobody shared with them — a wasted query at
	// best, and an invitation to project an owner's row through a viewer mask
	// at worst.
	t.Run("the merge leg is still participant-only", func(t *testing.T) {
		ids, err := repo.ActiveTripVehicleIDs(ctx, shareOwnerA)
		if err != nil {
			t.Fatalf("ActiveTripVehicleIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("the owner's own car leaked into the merge leg: %v", ids)
		}
	})
}

// TestActiveTripIDsForUserRespectsTheWindowAndTheShare: the annotation is an
// access-carrying value, so it must obey the same two predicates every other
// trips surface does.
func TestActiveTripIDsForUserRespectsTheWindowAndTheShare(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareID := seedTripFixture(t)
	repo := newTripRepo(t)
	shareRepo := newShareRepo(t)
	now := time.Now().UTC()

	// A window that has not opened yet: nobody, owner included.
	future := mustCreateTrip(t, repo, vehicleID, now.Add(time.Hour), now.Add(2*time.Hour), []string{shareID})
	for _, user := range []string{shareOwnerA, shareViewer1} {
		ids, err := repo.ActiveTripIDsForUser(ctx, user)
		if err != nil {
			t.Fatalf("ActiveTripIDsForUser(%s): %v", user, err)
		}
		if len(ids) != 0 {
			t.Fatalf("a scheduled window is reported active for %s: %v", user, ids)
		}
	}
	if err := repo.Delete(ctx, future.ID, shareOwnerA); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// An open window, then the share revoked: the OWNER keeps their own
	// annotation (they hold no grant), the participant loses theirs.
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(time.Hour), []string{shareID})
	if _, err := shareRepo.RevokeInvite(ctx, shareID, shareOwnerA); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}

	ownerIDs, err := repo.ActiveTripIDsForUser(ctx, shareOwnerA)
	if err != nil {
		t.Fatalf("ActiveTripIDsForUser(owner): %v", err)
	}
	if ownerIDs[vehicleID] != trip.ID {
		t.Fatalf("the owner lost their own window when a guest's share was revoked: %v", ownerIDs)
	}
	viewerIDs, err := repo.ActiveTripIDsForUser(ctx, shareViewer1)
	if err != nil {
		t.Fatalf("ActiveTripIDsForUser(viewer): %v", err)
	}
	if len(viewerIDs) != 0 {
		t.Fatalf("a revoked share still carries activeTripId: %v", viewerIDs)
	}
}
