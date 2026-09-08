package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-618 against a real Postgres: a live participant widens a roster, and the
// three things only a database can prove — the live-grant predicate on the
// target, the attribution column's preserve-on-no-op rule, and the audit row.

// seedSecondViewer adds a SECOND accepted grant on the same vehicle, held by
// shareViewer2, and returns its share id.
//
// The whole feature needs two share-holders: one to BE the participant doing the
// adding, and one to be added. A fixture with a single grant can only test the
// owner's path under another name.
func seedSecondViewer(t *testing.T, vehicleID string) string {
	t.Helper()
	shareRepo := newShareRepo(t)
	invite := mustCreateInvite(t, shareRepo, shareOwnerA, vehicleID, []string{vehicleID}, store.SharePermissionLive)
	if _, err := shareRepo.RedeemCode(context.Background(), invite.Code, shareViewer2); err != nil {
		t.Fatalf("RedeemCode(viewer2): %v", err)
	}
	return invite.ID
}

// suspendShare stamps `suspended_at` directly.
//
// A raw UPDATE rather than the repository's own suspend path, deliberately:
// what is under test is that the ADD refuses a suspended grant, and reaching
// that state through the share repository would make this test fail if THAT
// path broke, for a reason that has nothing to do with trips.
func suspendShare(t *testing.T, shareID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_vehicle_shares SET suspended_at = NOW() WHERE id = $1`, shareID); err != nil {
		t.Fatalf("suspend share: %v", err)
	}
}

// participantAddedAuditRows returns the `trip.participant_added` rows filed
// against one trip, newest first, as (actorUserID, metadata).
func participantAddedAuditRows(t *testing.T, tripID string) []struct {
	Actor    string
	Metadata map[string]string
} {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
SELECT "userId", "metadata" FROM "AuditLog"
WHERE action = 'trip.participant_added' AND "targetId" = $1
ORDER BY "timestamp" DESC`, tripID)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer rows.Close()

	var out []struct {
		Actor    string
		Metadata map[string]string
	}
	for rows.Next() {
		var (
			actor string
			raw   []byte
		)
		if err := rows.Scan(&actor, &raw); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		meta := map[string]string{}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("decode audit metadata: %v", err)
		}
		out = append(out, struct {
			Actor    string
			Metadata map[string]string
		}{Actor: actor, Metadata: meta})
	}
	return out
}

// TestTripRepo_ParticipantAddsSomebodyWhoAlreadyHasTheCar is the headline path.
func TestTripRepo_ParticipantAddsSomebodyWhoAlreadyHasTheCar(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})

	// The ACTOR is viewer 1 — a participant, not the owner.
	view, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareTwo})
	if err != nil {
		t.Fatalf("AddParticipants: %v", err)
	}
	if len(view.Participants) != 2 {
		t.Fatalf("roster = %d, want 2", len(view.Participants))
	}
	// The view is read back for the ACTOR, so their own role must resolve.
	if view.Role != "participant" {
		t.Errorf("role = %q, want participant — the read-back is for the caller", view.Role)
	}

	// THE ATTRIBUTION. The added person's row names the participant who added
	// them; the person the OWNER put on at create time names the owner, and
	// resolves to nil here only because these fixture accounts have no confirmed
	// name — which is itself the MYR-583 gate doing its job.
	var addedBy *string
	if err := testPool.QueryRow(ctx, `
SELECT added_by_user_id FROM go_trip_participants
WHERE trip_id = $1 AND user_id = $2`, trip.ID, shareViewer2).Scan(&addedBy); err != nil {
		t.Fatalf("read attribution: %v", err)
	}
	if addedBy == nil || *addedBy != shareViewer1 {
		t.Errorf("added_by_user_id = %v, want the acting participant %q", addedBy, shareViewer1)
	}

	// THE AUDIT ROW, with the ACTOR as its subject.
	audit := participantAddedAuditRows(t, trip.ID)
	if len(audit) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 (create writes none)", len(audit))
	}
	if audit[0].Actor != shareViewer1 {
		t.Errorf("audit userId = %q, want the actor %q", audit[0].Actor, shareViewer1)
	}
	// TWO OPAQUE CUIDS AND NOTHING ELSE (CG-DL-5). No trip name, no user id for
	// the person who was added, no names of any kind.
	if len(audit[0].Metadata) != 2 ||
		audit[0].Metadata["vehicleId"] != vehicleID ||
		audit[0].Metadata["shareId"] != shareTwo {
		t.Errorf("audit metadata = %v, want exactly {vehicleId, shareId}", audit[0].Metadata)
	}
}

// TestTripRepo_AddRefusesASuspendedGrant. The predicate is the SAME one the
// owner's add uses, and it must not soften because a participant is asking.
func TestTripRepo_AddRefusesASuspendedGrant(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})
	suspendShare(t, shareTwo)

	_, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareTwo})
	if !errors.Is(err, store.ErrTripParticipantNotShared) {
		t.Fatalf("err = %v, want ErrTripParticipantNotShared", err)
	}

	// NOTHING WAS APPLIED. All-or-nothing is what makes the refusal safe to
	// retry, and a partially-applied add would be invisible to the caller.
	view, err := repo.GetForUser(ctx, trip.ID, shareOwnerA)
	if err != nil {
		t.Fatalf("GetForUser: %v", err)
	}
	if len(view.Participants) != 1 {
		t.Errorf("roster = %d, want 1 — the refused add must have written nothing", len(view.Participants))
	}
	if rows := participantAddedAuditRows(t, trip.ID); len(rows) != 0 {
		t.Errorf("a refused add wrote %d audit rows", len(rows))
	}
}

// TestTripRepo_AddIsRefusedToAStranger. The 404-not-403 rule, enforced by the
// role probe rather than by the handler: somebody with no relationship to the
// trip gets the same answer an unknown id gets.
func TestTripRepo_AddIsRefusedToAStranger(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})

	// shareViewer2 HOLDS A GRANT ON THE CAR and is still not on the trip, which
	// is the interesting case: access to the vehicle is not access to a window.
	if _, err := repo.AddParticipants(ctx, trip.ID, shareViewer2, []string{shareTwo}); !errors.Is(err, store.ErrTripNotFound) {
		t.Fatalf("err = %v, want ErrTripNotFound for a non-member who holds a share", err)
	}
	if _, err := repo.AddParticipants(ctx, "ctrp_nope", shareViewer1, []string{shareTwo}); !errors.Is(err, store.ErrTripNotFound) {
		t.Fatalf("err = %v, want ErrTripNotFound for an unknown trip", err)
	}
}

// TestTripRepo_AddRefusesAnEndedTrip. Adding somebody to a closed window grants
// nothing, so the honest answer is that the trip is over.
func TestTripRepo_AddRefusesAnEndedTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})
	if _, err := repo.End(ctx, trip.ID, shareOwnerA); err != nil {
		t.Fatalf("End: %v", err)
	}

	if _, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareTwo}); !errors.Is(err, store.ErrTripEnded) {
		t.Fatalf("err = %v, want ErrTripEnded", err)
	}
}

// TestTripRepo_ReAddingSomebodyPresentIsASilentNoOp, and — the part that needs a
// database — it does NOT rewrite who put them there.
//
// ⚠ THIS IS THE ATTRIBUTION'S SECURITY PROPERTY. Without the preserve-on-live
// arm in the upsert, any participant could claim credit for the owner's own
// additions simply by adding the same person a second time.
func TestTripRepo_ReAddingSomebodyPresentIsASilentNoOp(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	// The OWNER puts viewer 1 on at create time, so the attribution is theirs.
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})

	// Viewer 1 now "adds" themselves again.
	if _, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareOne}); err != nil {
		t.Fatalf("AddParticipants: %v", err)
	}

	var addedBy *string
	if err := testPool.QueryRow(ctx, `
SELECT added_by_user_id FROM go_trip_participants
WHERE trip_id = $1 AND user_id = $2`, trip.ID, shareViewer1).Scan(&addedBy); err != nil {
		t.Fatalf("read attribution: %v", err)
	}
	if addedBy == nil || *addedBy != shareOwnerA {
		t.Errorf("added_by_user_id = %v, want the OWNER %q — a re-add must not "+
			"rewrite the attribution", addedBy, shareOwnerA)
	}
	if rows := participantAddedAuditRows(t, trip.ID); len(rows) != 0 {
		t.Errorf("a no-op re-add wrote %d audit rows, want 0", len(rows))
	}
}

// TestTripRepo_OwnerPatchAlsoAttributesAndAudits. The owner's add goes through
// the same body, so it writes the same column and the same row — an owner's add
// and a participant's add differ in who may ask, and in nothing that reaches the
// database.
func TestTripRepo_OwnerPatchAlsoAttributesAndAudits(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})

	if _, err := repo.Update(ctx, trip.ID, shareOwnerA, store.UpdateTripInput{
		AddParticipantIDs: []string{shareTwo},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	audit := participantAddedAuditRows(t, trip.ID)
	if len(audit) != 1 || audit[0].Actor != shareOwnerA {
		t.Fatalf("audit rows = %+v, want one filed against the owner", audit)
	}
	var addedBy *string
	if err := testPool.QueryRow(ctx, `
SELECT added_by_user_id FROM go_trip_participants
WHERE trip_id = $1 AND user_id = $2`, trip.ID, shareViewer2).Scan(&addedBy); err != nil {
		t.Fatalf("read attribution: %v", err)
	}
	if addedBy == nil || *addedBy != shareOwnerA {
		t.Errorf("added_by_user_id = %v, want %q", addedBy, shareOwnerA)
	}
}

// TestTripRepo_AddablePeople is §7.30.11's whole predicate in one test: present
// people are excluded, suspended grants are excluded, and a stranger gets the
// 404 rather than an empty list.
func TestTripRepo_AddablePeople(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})

	t.Run("offers the grant-holder who is not on the trip", func(t *testing.T) {
		// Read as the PARTICIPANT, which is the caller the route exists for.
		people, err := repo.AddablePeople(ctx, trip.ID, shareViewer1)
		if err != nil {
			t.Fatalf("AddablePeople: %v", err)
		}
		if len(people) != 1 || people[0].ShareID != shareTwo {
			t.Fatalf("people = %+v, want exactly the second grant %q", people, shareTwo)
		}
		// The name falls back to the owner's own label when the grantee has no
		// confirmed name — the roster rule, applied identically here so a
		// person is not called one thing in the picker and another on the trip.
		if people[0].DisplayName != "Mira Chen" {
			t.Errorf("displayName = %q, want the grant label", people[0].DisplayName)
		}
	})

	t.Run("the owner sees the same list", func(t *testing.T) {
		people, err := repo.AddablePeople(ctx, trip.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("AddablePeople(owner): %v", err)
		}
		if len(people) != 1 || people[0].ShareID != shareTwo {
			t.Errorf("owner's list = %+v, want the same single row", people)
		}
	})

	t.Run("a suspended grant is not offered", func(t *testing.T) {
		suspendShare(t, shareTwo)
		defer func() {
			if _, err := testPool.Exec(ctx,
				`UPDATE go_vehicle_shares SET suspended_at = NULL WHERE id = $1`, shareTwo); err != nil {
				t.Fatalf("restore share: %v", err)
			}
		}()

		people, err := repo.AddablePeople(ctx, trip.ID, shareViewer1)
		if err != nil {
			t.Fatalf("AddablePeople: %v", err)
		}
		if len(people) != 0 {
			t.Errorf("people = %+v, want none — offering a suspended grant would "+
				"produce a refusal the person could not explain", people)
		}
	})

	t.Run("somebody already on the trip is not offered again", func(t *testing.T) {
		if _, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareTwo}); err != nil {
			t.Fatalf("AddParticipants: %v", err)
		}
		defer func() {
			if _, err := repo.Update(ctx, trip.ID, shareOwnerA, store.UpdateTripInput{
				RemoveParticipantIDs: []string{shareTwo},
			}); err != nil {
				t.Fatalf("restore roster: %v", err)
			}
		}()

		people, err := repo.AddablePeople(ctx, trip.ID, shareViewer1)
		if err != nil {
			t.Fatalf("AddablePeople: %v", err)
		}
		if len(people) != 0 {
			t.Errorf("people = %+v, want none — everybody is aboard", people)
		}
	})

	t.Run("a stranger gets 404, not an empty list", func(t *testing.T) {
		// The distinction matters: an empty list is also the honest answer for a
		// trip where everybody is aboard, so without the role probe the two
		// would be indistinguishable and a stranger would receive a 200.
		if _, err := repo.AddablePeople(ctx, trip.ID, shareOwnerB); !errors.Is(err, store.ErrTripNotFound) {
			t.Fatalf("err = %v, want ErrTripNotFound", err)
		}
	})

	t.Run("somebody who LEFT can be offered again", func(t *testing.T) {
		if _, err := repo.Leave(ctx, trip.ID, shareViewer1); err != nil {
			t.Fatalf("Leave: %v", err)
		}
		defer func() {
			if _, err := repo.Update(ctx, trip.ID, shareOwnerA, store.UpdateTripInput{
				AddParticipantIDs: []string{shareOne},
			}); err != nil {
				t.Fatalf("restore roster: %v", err)
			}
		}()

		// Read as the OWNER — viewer 1 has just left and is no longer entitled.
		people, err := repo.AddablePeople(ctx, trip.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("AddablePeople: %v", err)
		}
		if len(people) != 2 {
			t.Errorf("people = %+v, want 2 — a departed participant is addable again", people)
		}
	})
}

// TestTripRepo_RosterCarriesTheAdderName proves the ladder resolves through the
// confirmation gate rather than reading a raw name column.
func TestTripRepo_RosterCarriesTheAdderName(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})

	// UNCONFIRMED first: a name exists on the account and must still resolve to
	// nil, because MYR-583's gate is what decides whether a counterparty sees it.
	//
	// The name is written to the ladder's TOP rung ("User"."name"), which is
	// where seedShareFixtures already puts one for every fixture account — a
	// go_users row would be the third rung and could never be reached, so a
	// test seeded there would be asserting against a name it did not set.
	if _, err := testPool.Exec(ctx,
		`UPDATE "User" SET "name" = 'Nabil Ahmed' WHERE "id" = $1`, shareViewer1); err != nil {
		t.Fatalf("seed adder name: %v", err)
	}
	if _, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareTwo}); err != nil {
		t.Fatalf("AddParticipants: %v", err)
	}

	roster := rosterEntryFor(t, repo, trip.ID, shareViewer2)
	if roster.AddedByName != nil {
		t.Fatalf("addedByName = %q for an UNCONFIRMED adder, want nil", *roster.AddedByName)
	}

	// Confirm the name and the same row resolves.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_profile_name_confirmations (user_id) VALUES ($1)
		 ON CONFLICT (user_id) DO NOTHING`, shareViewer1); err != nil {
		t.Fatalf("confirm adder name: %v", err)
	}
	roster = rosterEntryFor(t, repo, trip.ID, shareViewer2)
	if roster.AddedByName == nil {
		t.Fatalf("addedByName = nil for a CONFIRMED adder, want the first token Nabil")
	}
	if *roster.AddedByName != "Nabil" {
		t.Errorf("addedByName = %q, want the FIRST token Nabil", *roster.AddedByName)
	}
}

// rosterEntryFor reads one roster row through the repository's own view builder.
func rosterEntryFor(t *testing.T, repo *store.TripRepo, tripID, userID string) store.TripParticipantView {
	t.Helper()
	view, err := repo.GetForUser(context.Background(), tripID, shareOwnerA)
	if err != nil {
		t.Fatalf("GetForUser: %v", err)
	}
	for _, p := range view.Participants {
		if p.UserID == userID {
			return p
		}
	}
	t.Fatalf("no roster row for %q in %+v", userID, view.Participants)
	return store.TripParticipantView{}
}

// TestTripRepo_SuspendedParticipantCannotActOnTheTrip is the MYR-618 REVIEW
// FIX, and it is the sharpest test in this file.
//
// A participant's membership row is NOT rewritten when their grant on the car
// is suspended or revoked — the cascade that stamps `left_at` is a display
// repair (trips.md §6) and nothing runs it on a suspend at all. So the roster
// row survives, and a role probe that tested `left_at IS NULL` ALONE would keep
// resolving `participant` for somebody the owner has already cut off: their map
// has gone dark, and they could still widen the owner's roster and enumerate
// the car's grant-holders by name.
//
// `tripMemberRoleExpr` is what closes that, by re-joining the live grant in the
// probe itself (invariant 3). The owner's own access is unaffected — an owner
// holds no grant on their own car — which is the other half of the assertion.
func TestTripRepo_SuspendedParticipantCannotActOnTheTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})

	t.Run("while the grant is live the participant may act", func(t *testing.T) {
		if _, err := repo.AddablePeople(ctx, trip.ID, shareViewer1); err != nil {
			t.Fatalf("AddablePeople: %v", err)
		}
	})

	t.Run("a SUSPENDED grant-holder gets the stranger's answer", func(t *testing.T) {
		suspendShare(t, shareOne)

		// The membership row is UNTOUCHED — that is the precondition this test
		// exists for, so assert it rather than assume it.
		var leftAt *time.Time
		if err := testPool.QueryRow(ctx, `
SELECT left_at FROM go_trip_participants WHERE trip_id = $1 AND user_id = $2`,
			trip.ID, shareViewer1).Scan(&leftAt); err != nil {
			t.Fatalf("read membership: %v", err)
		}
		if leftAt != nil {
			t.Fatalf("left_at = %v — a suspend must not touch the roster row", leftAt)
		}

		if _, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareTwo}); !errors.Is(err, store.ErrTripNotFound) {
			t.Errorf("AddParticipants err = %v, want ErrTripNotFound for a suspended grant-holder", err)
		}
		if _, err := repo.AddablePeople(ctx, trip.ID, shareViewer1); !errors.Is(err, store.ErrTripNotFound) {
			t.Errorf("AddablePeople err = %v, want ErrTripNotFound for a suspended grant-holder", err)
		}
	})

	t.Run("the OWNER still succeeds", func(t *testing.T) {
		if _, err := repo.AddablePeople(ctx, trip.ID, shareOwnerA); err != nil {
			t.Fatalf("AddablePeople(owner): %v — an owner holds no grant on their own car", err)
		}
		if _, err := repo.AddParticipants(ctx, trip.ID, shareOwnerA, []string{shareTwo}); err != nil {
			t.Fatalf("AddParticipants(owner): %v", err)
		}
	})
}

// TestTripRepo_RevokedParticipantCannotActOnTheTrip is the same escalation
// through the OTHER half of invariant 3's predicate.
//
// Revocation is a tombstone flip on the grant, and the cascade that would stamp
// `left_at` is a separate call the owner's revoke now makes — but this test
// deliberately does NOT run it, because the security property must not depend
// on a repair having happened.
func TestTripRepo_RevokedParticipantCannotActOnTheTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})

	// The raw tombstone flip, NOT VehicleShareRepo.RevokeInvite — that call now
	// runs the roster cascade in the same transaction, which would stamp
	// `left_at` and let this test pass for the wrong reason.
	if _, err := testPool.Exec(ctx,
		`UPDATE go_vehicle_shares SET status = 'revoked', revoked_at = NOW() WHERE id = $1`, shareOne); err != nil {
		t.Fatalf("revoke share: %v", err)
	}

	if _, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareTwo}); !errors.Is(err, store.ErrTripNotFound) {
		t.Errorf("AddParticipants err = %v, want ErrTripNotFound for a revoked grant-holder", err)
	}
	if _, err := repo.AddablePeople(ctx, trip.ID, shareViewer1); !errors.Is(err, store.ErrTripNotFound) {
		t.Errorf("AddablePeople err = %v, want ErrTripNotFound for a revoked grant-holder", err)
	}
}

// TestTripRepo_OwnersRemoveSticks is the MYR-618 REVIEW FIX for the roster's
// second hole: the upsert REVIVES a departed membership, so before migration
// 0061 any participant could undo the owner's remove — the one roster verb this
// feature deliberately kept owner-only — simply by re-sending the same share id.
func TestTripRepo_OwnersRemoveSticks(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour),
		[]string{shareOne, shareTwo})

	// The OWNER removes viewer 2.
	if _, err := repo.Update(ctx, trip.ID, shareOwnerA, store.UpdateTripInput{
		RemoveParticipantIDs: []string{shareTwo},
	}); err != nil {
		t.Fatalf("Update(remove): %v", err)
	}

	t.Run("the marker is on the row", func(t *testing.T) {
		var removedByOwner bool
		if err := testPool.QueryRow(ctx, `
SELECT removed_by_owner FROM go_trip_participants WHERE trip_id = $1 AND user_id = $2`,
			trip.ID, shareViewer2).Scan(&removedByOwner); err != nil {
			t.Fatalf("read marker: %v", err)
		}
		if !removedByOwner {
			t.Fatal("removed_by_owner = false after an owner's remove")
		}
	})

	t.Run("a participant cannot add them back", func(t *testing.T) {
		_, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareTwo})
		if !errors.Is(err, store.ErrTripParticipantOwnerRemoved) {
			t.Fatalf("err = %v, want ErrTripParticipantOwnerRemoved", err)
		}
		// AND NOTHING WAS APPLIED — the refusal is all-or-nothing, so the
		// transaction rolled back and the row is still departed.
		var leftAt *time.Time
		if err := testPool.QueryRow(ctx, `
SELECT left_at FROM go_trip_participants WHERE trip_id = $1 AND user_id = $2`,
			trip.ID, shareViewer2).Scan(&leftAt); err != nil {
			t.Fatalf("read membership: %v", err)
		}
		if leftAt == nil {
			t.Fatal("the refused add revived the row anyway")
		}
	})

	t.Run("a participant's picker does not offer them", func(t *testing.T) {
		people, err := repo.AddablePeople(ctx, trip.ID, shareViewer1)
		if err != nil {
			t.Fatalf("AddablePeople(participant): %v", err)
		}
		for _, p := range people {
			if p.ShareID == shareTwo {
				t.Fatalf("a participant's picker offered an owner-removed person: %+v", people)
			}
		}
	})

	t.Run("the OWNER's picker still offers them", func(t *testing.T) {
		// Otherwise the removal would be irreversible: the owner is the only
		// person who may undo it, and a picker that hid the row would strand it.
		people, err := repo.AddablePeople(ctx, trip.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("AddablePeople(owner): %v", err)
		}
		found := false
		for _, p := range people {
			if p.ShareID == shareTwo {
				found = true
			}
		}
		if !found {
			t.Fatalf("the owner's picker withheld their own removal: %+v", people)
		}
	})

	t.Run("the OWNER re-adds them, and the marker clears", func(t *testing.T) {
		if _, err := repo.Update(ctx, trip.ID, shareOwnerA, store.UpdateTripInput{
			AddParticipantIDs: []string{shareTwo},
		}); err != nil {
			t.Fatalf("Update(re-add): %v", err)
		}
		var removedByOwner bool
		var leftAt *time.Time
		if err := testPool.QueryRow(ctx, `
SELECT removed_by_owner, left_at FROM go_trip_participants WHERE trip_id = $1 AND user_id = $2`,
			trip.ID, shareViewer2).Scan(&removedByOwner, &leftAt); err != nil {
			t.Fatalf("read row: %v", err)
		}
		if removedByOwner || leftAt != nil {
			t.Fatalf("after the owner's re-add: removed_by_owner=%v left_at=%v, want false/nil",
				removedByOwner, leftAt)
		}

		// And a participant may now re-add them freely, which is the proof the
		// gate is about the OWNER'S DECISION and not about the person.
		if _, err := repo.Leave(ctx, trip.ID, shareViewer2); err != nil {
			t.Fatalf("Leave: %v", err)
		}
		if _, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareTwo}); err != nil {
			t.Fatalf("AddParticipants after a self-leave: %v", err)
		}
	})
}

// TestTripRepo_SelfLeaveIsNotARemoval pins the other side of migration 0061: a
// person who walks away has NOT been removed, so any member may invite them
// back. Stamping the marker on a self-leave would make leaving irreversible by
// anybody but the owner, which is the opposite of what leaving means.
func TestTripRepo_SelfLeaveIsNotARemoval(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour),
		[]string{shareOne, shareTwo})

	if _, err := repo.Leave(ctx, trip.ID, shareViewer2); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	var removedByOwner bool
	if err := testPool.QueryRow(ctx, `
SELECT removed_by_owner FROM go_trip_participants WHERE trip_id = $1 AND user_id = $2`,
		trip.ID, shareViewer2).Scan(&removedByOwner); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if removedByOwner {
		t.Fatal("a self-leave stamped removed_by_owner")
	}

	people, err := repo.AddablePeople(ctx, trip.ID, shareViewer1)
	if err != nil {
		t.Fatalf("AddablePeople: %v", err)
	}
	found := false
	for _, p := range people {
		if p.ShareID == shareTwo {
			found = true
		}
	}
	if !found {
		t.Fatalf("somebody who LEFT is not offered again: %+v", people)
	}
	if _, err := repo.AddParticipants(ctx, trip.ID, shareViewer1, []string{shareTwo}); err != nil {
		t.Fatalf("AddParticipants after a self-leave: %v", err)
	}
}

// TestTripRepo_AddablePeopleRefusesAnEndedTrip is review finding 4: the probe
// already knew the window had closed and threw the answer away, so the picker
// listed names the very next PATCH would refuse with `trip_ended`.
func TestTripRepo_AddablePeopleRefusesAnEndedTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})
	if _, err := repo.End(ctx, trip.ID, shareOwnerA); err != nil {
		t.Fatalf("End: %v", err)
	}

	for _, actor := range []struct{ name, userID string }{
		{"owner", shareOwnerA},
		{"participant", shareViewer1},
	} {
		if _, err := repo.AddablePeople(ctx, trip.ID, actor.userID); !errors.Is(err, store.ErrTripEnded) {
			t.Errorf("AddablePeople(%s) err = %v, want ErrTripEnded", actor.name, err)
		}
	}
}

// TestTripRepo_AddAndRemoveInOnePatchAuditsNothing is review finding 7.
//
// The documented rule is that an id in BOTH lists ends up REMOVED, and the end
// state was always right because the remove lands second. What was wrong is
// everything the add did on its way past: a `trip.participant_added` audit row
// for a person the same request took off, the attribution column stamped, and
// the person momentarily on the roster the handler diffs for its push fan-out —
// so the owner could have been told somebody joined a trip they were never on.
func TestTripRepo_AddAndRemoveInOnePatchAuditsNothing(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehicleID, shareOne := seedTripFixture(t)
	shareTwo := seedSecondViewer(t, vehicleID)
	repo := newTripRepo(t)

	now := time.Now().UTC()
	trip := mustCreateTrip(t, repo, vehicleID, now.Add(-time.Hour), now.Add(24*time.Hour), []string{shareOne})

	view, err := repo.Update(ctx, trip.ID, shareOwnerA, store.UpdateTripInput{
		AddParticipantIDs:    []string{shareTwo},
		RemoveParticipantIDs: []string{shareTwo},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	for _, p := range view.Participants {
		if p.ParticipantID == shareTwo {
			t.Fatalf("the contradictory id is on the roster: %+v", view.Participants)
		}
	}
	if rows := participantAddedAuditRows(t, trip.ID); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none — the add never survived its own transaction", rows)
	}
	// The membership row must not exist at all: the add was skipped, so there
	// was nothing for the remove to tombstone either.
	var exists bool
	if err := testPool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM go_trip_participants WHERE trip_id = $1 AND user_id = $2)`,
		trip.ID, shareViewer2).Scan(&exists); err != nil {
		t.Fatalf("probe membership: %v", err)
	}
	if exists {
		t.Error("the add wrote a membership row the same request removed")
	}
}
