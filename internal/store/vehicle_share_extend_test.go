package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// MYR-609 ExtendShare — copying an accepted grant onto a second car the same
// owner owns. Real Postgres throughout, because what is under test is a set of
// conditional predicates, a partial-unique index and an audit row inside one
// transaction, none of which a mock exercises.

// extendedGrantOn returns the single live row on vehicleID, failing if there is
// not exactly one. Read back through the repository's own listing so the
// assertions see what an owner's Share tab would.
func extendedGrantOn(t *testing.T, repo *store.VehicleShareRepo, vehicleID string) store.VehicleShare {
	t.Helper()
	rows, err := repo.ListInvitesForVehicle(context.Background(), vehicleID, shareOwnerA)
	if err != nil {
		t.Fatalf("ListInvitesForVehicle(%s): %v", vehicleID, err)
	}
	if len(rows) != 1 {
		t.Fatalf("vehicle %s has %d live rows, want exactly 1", vehicleID, len(rows))
	}
	return rows[0]
}

// TestVehicleShareRepo_ExtendShare_CopiesTheRow is the shape assertion: the new
// row must be indistinguishable from an ordinary accepted grant, because every
// access gate treats it as one.
func TestVehicleShareRepo_ExtendShare_CopiesTheRow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	cleanVehicleShares(t)
	cleanAuditLog(t, testPool)
	sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionRides)
	source := extendedGrantOn(t, repo, vehA1)

	row, err := repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID:     shareOwnerA,
		TargetVehicleID: vehA2,
		SourceShareID:   sourceID,
	})
	if err != nil {
		t.Fatalf("ExtendShare: %v", err)
	}

	if row.VehicleID != vehA2 {
		t.Errorf("vehicle = %s, want the TARGET %s", row.VehicleID, vehA2)
	}
	if row.ID == sourceID {
		t.Error("the extended grant reused the source row's id; it must be a new row")
	}
	if row.Status != store.ShareStatusAccepted {
		t.Errorf("status = %q, want accepted — an extended grant is live immediately, "+
			"there is no code for anybody to redeem", row.Status)
	}
	if row.AcceptedByUserID != shareViewer1 {
		t.Errorf("accepted_by_user_id = %q, want the source's grantee %q",
			row.AcceptedByUserID, shareViewer1)
	}
	if row.AcceptedAt == nil {
		t.Error("accepted_at is nil; an accepted row must carry the instant its access began")
	}
	// The three copied values. label is P1, so a mismatch reports lengths.
	if row.Label != source.Label {
		t.Errorf("label was not copied from the source grant (lengths %d vs %d)",
			len(row.Label), len(source.Label))
	}
	if row.Permission != source.Permission {
		t.Errorf("permission = %q, want the source's %q", row.Permission, source.Permission)
	}
	if !row.AllowRides {
		t.Error("allow_rides = false; the source grant carried the ride capability and it must " +
			"be copied, not re-derived from a preset")
	}
	if row.SuspendedAt != nil {
		t.Error("the extended grant is born suspended; the source was live")
	}
	// NO CREDENTIAL IS WRITTEN AT ALL (migration 0052). Not a minted-and-lapsed
	// one that three predicates agree is dead — a NULL. The value would be P1
	// and is not reported here even on failure.
	if row.Code != "" {
		t.Error("an accepted row carried a `code` out of the repository; the SQL projection " +
			"blanks it, and since MYR-609 there is nothing to blank")
	}
	if !row.ExpiresAt.IsZero() {
		t.Errorf("expires_at = %v, want the zero value — an extended grant has no deadline "+
			"because it has no credential", row.ExpiresAt)
	}
	t.Run("and the columns are NULL in the database, not merely suppressed on read", func(t *testing.T) {
		var codeNull, expiryNull bool
		if err := testPool.QueryRow(ctx,
			`SELECT code IS NULL, expires_at IS NULL FROM go_vehicle_shares WHERE id = $1`, row.ID).
			Scan(&codeNull, &expiryNull); err != nil {
			t.Fatalf("read the stored row: %v", err)
		}
		if !codeNull {
			t.Error("the extended row stores a code. A credential whose deadness rests on three " +
				"unrelated predicates continuing to agree is a credential")
		}
		if !expiryNull {
			t.Error("the extended row stores an expires_at; a deadline is a property of a credential")
		}
	})

	t.Run("the source grant is untouched", func(t *testing.T) {
		again := extendedGrantOn(t, repo, vehA1)
		if again.ID != sourceID || again.Status != store.ShareStatusAccepted {
			t.Errorf("source row changed: id=%s status=%s", again.ID, again.Status)
		}
	})
}

// A SUSPENDED source is REFUSED, and the pause is NOT copied forward.
//
// The tempting reading is the opposite one, and the PR shipped it: refusing
// seems to force an owner to un-pause somebody in order to add a car. What
// copying it actually produces is a grant born suspended — excluded from the
// access set by the suspension invariant, so INVISIBLE to the grantee, while
// the owner's own listing shows the car as shared with them. Nobody would know
// to lift it, and it contradicts the invariant queryAcceptSharesByID asserts in
// the other direction (`suspended_at = NULL`: a freshly accepted grant is never
// born paused, precisely so no grant exists that only its creator can see).
func TestVehicleShareRepo_ExtendShare_RefusesSuspendedSource(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	cleanVehicleShares(t)
	cleanAuditLog(t, testPool)
	sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionRides)
	if _, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
		InviteID: sourceID, OwnerUserID: shareOwnerA, Suspended: boolPtr(true),
	}); err != nil {
		t.Fatalf("suspend source: %v", err)
	}

	_, err := repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID:     shareOwnerA,
		TargetVehicleID: vehA2,
		SourceShareID:   sourceID,
	})
	if !errors.Is(err, store.ErrShareSourceSuspended) {
		t.Fatalf("err = %v, want ErrShareSourceSuspended", err)
	}
	if errors.Is(err, store.ErrShareAlreadyGranted) {
		t.Error("a paused source must not borrow the already_shared sub-code; that one tells a " +
			"client the call SUCCEEDED")
	}
	if got := countQuery(t, `SELECT count(*) FROM go_vehicle_shares WHERE vehicle_id = $1`, vehA2); got != 0 {
		t.Errorf("%d rows were written on the target, want 0", got)
	}
	if got := countQuery(t, `SELECT count(*) FROM "AuditLog" WHERE "action" = $1`,
		string(store.AuditActionShareExtended)); got != 0 {
		t.Errorf("%d share.extended audit rows for a refused extend, want 0", got)
	}

	t.Run("and lifting the pause makes it extendable, with the new row LIVE", func(t *testing.T) {
		if _, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: sourceID, OwnerUserID: shareOwnerA, Suspended: boolPtr(false),
		}); err != nil {
			t.Fatalf("un-suspend source: %v", err)
		}
		row, err := repo.ExtendShare(ctx, store.ExtendShareInput{
			OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
		})
		if err != nil {
			t.Fatalf("ExtendShare after lifting the pause: %v", err)
		}
		if row.SuspendedAt != nil {
			t.Fatal("the extended grant is born suspended")
		}
		if ids := authAccessSet(t, shareViewer1); !slices.Contains(ids, vehA2) {
			t.Errorf("access set %v omits %s; the new grant is live and must convey", ids, vehA2)
		}
	})
}

// The new row must be admitted by the REAL access set — the owned-UNION-shared
// statement in internal/auth that the WebSocket subscribed set, GET
// /api/vehicles and every per-vehicle gate resolve through — with no special
// case for how the grant arrived.
func TestVehicleShareRepo_ExtendShare_AdmittedByTheAccessSet(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	cleanVehicleShares(t)
	sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionRides)

	before := authAccessSet(t, shareViewer1)
	if slices.Contains(before, vehA2) {
		t.Fatalf("fixture is wrong: %s is already in the viewer's access set", vehA2)
	}

	if _, err := repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID:     shareOwnerA,
		TargetVehicleID: vehA2,
		SourceShareID:   sourceID,
	}); err != nil {
		t.Fatalf("ExtendShare: %v", err)
	}

	after := authAccessSet(t, shareViewer1)
	if !slices.Contains(after, vehA2) {
		t.Errorf("access set %v omits %s; an extended grant is an accepted grant and must be "+
			"admitted like any other", after, vehA2)
	}
	if !slices.Contains(after, vehA1) {
		t.Errorf("access set %v lost the source car %s", after, vehA1)
	}

	// The capability gates read it too, not just the set.
	allowRides, err := repo.ShareGrantFor(ctx, shareViewer1, vehA2)
	if err != nil {
		t.Fatalf("ShareGrantFor on the extended grant: %v", err)
	}
	if !allowRides {
		t.Error("allow_rides = false at the capability gate; the source grant carried it")
	}
}

func TestVehicleShareRepo_ExtendShare_Refusals(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, vehB := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	// A live grant already on the target is the conflict, and so is extending a
	// grant onto its OWN vehicle — the second is a special case of the first,
	// which is why it answers 409 rather than a 404 that would hide a row the
	// caller demonstrably owns.
	t.Run("already shared", func(t *testing.T) {
		for _, tt := range []struct {
			name   string
			target func() string
		}{
			{"the grantee already holds a live grant on the target", func() string { return vehA2 }},
			{"the source grant is already on the target vehicle", func() string { return vehA1 }},
		} {
			t.Run(tt.name, func(t *testing.T) {
				cleanVehicleShares(t)
				sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)
				if tt.target() == vehA2 {
					if _, err := repo.ExtendShare(ctx, store.ExtendShareInput{
						OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
					}); err != nil {
						t.Fatalf("seed the first extend: %v", err)
					}
				}

				_, err := repo.ExtendShare(ctx, store.ExtendShareInput{
					OwnerUserID: shareOwnerA, TargetVehicleID: tt.target(), SourceShareID: sourceID,
				})
				if !errors.Is(err, store.ErrShareAlreadyGranted) {
					t.Fatalf("err = %v, want ErrShareAlreadyGranted", err)
				}
			})
		}
	})

	// Every unextendable source collapses onto ONE sentinel. Distinguishing
	// them would make the endpoint an oracle for other owners' invite ids.
	t.Run("an unextendable source is ErrShareNotFound, indistinguishably", func(t *testing.T) {
		tests := []struct {
			name   string
			source func(t *testing.T) string
		}{
			{"an id that never existed", func(*testing.T) string {
				return "cshmissing00000000000000000000ab"
			}},
			{"a PENDING invite nobody has redeemed", func(t *testing.T) string {
				t.Helper()
				return mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1},
					store.SharePermissionLive).ID
			}},
			{"a REVOKED tombstone", func(t *testing.T) string {
				t.Helper()
				id := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)
				if _, err := repo.RevokeInvite(ctx, id, shareOwnerA); err != nil {
					t.Fatalf("revoke: %v", err)
				}
				return id
			}},
			{"ANOTHER OWNER's accepted grant", func(t *testing.T) string {
				t.Helper()
				invite := mustCreateInvite(t, repo, shareOwnerB, vehB, []string{vehB},
					store.SharePermissionLive)
				if _, err := repo.RedeemCode(ctx, invite.Code, shareViewer1); err != nil {
					t.Fatalf("redeem owner B's invite: %v", err)
				}
				return invite.ID
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				cleanVehicleShares(t)
				sourceID := tt.source(t)

				_, err := repo.ExtendShare(ctx, store.ExtendShareInput{
					OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
				})
				if !errors.Is(err, store.ErrShareNotFound) {
					t.Fatalf("err = %v, want ErrShareNotFound", err)
				}
				if !errors.Is(err, sdk.ErrNotFound) {
					t.Error("the sentinel must wrap sdk.ErrNotFound; the handler's 404 branch keys on it")
				}
				if got := countQuery(t, `SELECT count(*) FROM go_vehicle_shares WHERE vehicle_id = $1`, vehA2); got != 0 {
					t.Errorf("%d rows were written on the target for a refused extend, want 0", got)
				}
			})
		}
	})

	// The repository re-checks target ownership inside the transaction even
	// though the handler already refused a non-owner with a 403. The handler's
	// check is the good error message; this one is what holds if a future
	// caller skips it.
	t.Run("a target vehicle the caller does not own is ErrShareVehicleNotOwned", func(t *testing.T) {
		cleanVehicleShares(t)
		sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)

		_, err := repo.ExtendShare(ctx, store.ExtendShareInput{
			OwnerUserID: shareOwnerA, TargetVehicleID: vehB, SourceShareID: sourceID,
		})
		if !errors.Is(err, store.ErrShareVehicleNotOwned) {
			t.Fatalf("err = %v, want ErrShareVehicleNotOwned", err)
		}
		if got := countQuery(t, `SELECT count(*) FROM go_vehicle_shares WHERE vehicle_id = $1`, vehB); got != 0 {
			t.Errorf("%d rows were written on another owner's car, want 0", got)
		}
	})

	// A malformed input is refused by the SAME PREDICATES that refuse a
	// well-formed one naming nothing, so it arrives as a MAPPED sentinel and
	// not as a bare error the handler could only turn into a 500. An earlier
	// cut had a Go-side `validateExtendInput` returning `errors.New`, and a
	// client sending `{"shareId": ""}` past the handler would have been told
	// the server had failed.
	t.Run("a malformed input is refused with a mapped sentinel", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			in   store.ExtendShareInput
			want error
		}{
			{"no owner", store.ExtendShareInput{TargetVehicleID: vehA2, SourceShareID: "x"},
				store.ErrShareNotFound},
			{"no source share", store.ExtendShareInput{OwnerUserID: shareOwnerA, TargetVehicleID: vehA2},
				store.ErrShareNotFound},
			{"no target vehicle", store.ExtendShareInput{OwnerUserID: shareOwnerA, SourceShareID: "x"},
				store.ErrShareNotFound},
		} {
			t.Run(tt.name, func(t *testing.T) {
				cleanVehicleShares(t)
				_, err := repo.ExtendShare(ctx, tt.in)
				if !errors.Is(err, tt.want) {
					t.Fatalf("err = %v, want %v — not a bare error that maps to 500", err, tt.want)
				}
			})
		}

		// With a REAL source in place, an empty target vehicle reaches the
		// ownership predicate instead and is 403's sentinel, not 404's.
		t.Run("a real source and no target vehicle", func(t *testing.T) {
			cleanVehicleShares(t)
			sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)
			_, err := repo.ExtendShare(ctx, store.ExtendShareInput{
				OwnerUserID: shareOwnerA, SourceShareID: sourceID,
			})
			if !errors.Is(err, store.ErrShareVehicleNotOwned) {
				t.Fatalf("err = %v, want ErrShareVehicleNotOwned", err)
			}
		})
	})
}

// The audit row is the ONLY record that this access was not redeemed, and its
// metadata is two opaque cuids (CG-DL-5). Written inside the same transaction
// as the grant, so neither can exist without the other.
func TestVehicleShareRepo_ExtendShare_WritesTheAuditRow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	cleanVehicleShares(t)
	cleanAuditLog(t, testPool)
	sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)

	row, err := repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
	})
	if err != nil {
		t.Fatalf("ExtendShare: %v", err)
	}

	var userID, action, targetType, targetID, initiator string
	var stamped time.Time
	var raw []byte
	if err := testPool.QueryRow(ctx,
		`SELECT "userId", "action", "targetType", "targetId", "initiator", "timestamp", "metadata"
		 FROM "AuditLog" WHERE "action" = $1`, string(store.AuditActionShareExtended)).
		Scan(&userID, &action, &targetType, &targetID, &initiator, &stamped, &raw); err != nil {
		t.Fatalf("read the share.extended audit row: %v", err)
	}

	if userID != shareOwnerA {
		t.Errorf("userId = %q, want the owner who acted %q", userID, shareOwnerA)
	}
	if action != "share.extended" {
		t.Errorf("action = %q, want share.extended", action)
	}
	if targetType != "vehicle" || targetID != vehA2 {
		t.Errorf("target = %s/%s, want vehicle/%s — the question this row answers is asked "+
			"about a car", targetType, targetID, vehA2)
	}
	if initiator != "user" {
		t.Errorf("initiator = %q, want user", initiator)
	}
	if stamped.IsZero() {
		t.Error("timestamp is the zero value")
	}

	// P0-ONLY AND CLOSED (CG-DL-5): exactly two keys, both opaque cuids. A
	// label or a code here would be a P1 value reaching permanent storage.
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode metadata: %v (raw %s)", err, raw)
	}
	if len(meta) != 2 {
		t.Errorf("metadata has %d keys (%v), want exactly 2", len(meta), meta)
	}
	if meta["shareId"] != row.ID {
		t.Errorf("metadata.shareId = %v, want the new grant %s", meta["shareId"], row.ID)
	}
	if meta["sourceShareId"] != sourceID {
		t.Errorf("metadata.sourceShareId = %v, want %s", meta["sourceShareId"], sourceID)
	}
	for k, v := range meta {
		if _, ok := v.(string); !ok {
			t.Errorf("metadata.%s = %v is not a string; every value here is an opaque cuid", k, v)
		}
	}
}

// A refused extend must leave NO audit row: the two go together or neither
// does, which is the whole reason the INSERT is inside the transaction.
//
// AND THE REFUSAL HERE IS THE INSERT-TIME ONE, deliberately. The pre-insert
// probe refuses BEFORE the audit row is written, so a test that trips it proves
// only that the audit write is ordered after the probe — it never exercises the
// rollback. The path that matters is the one the probe structurally cannot
// cover: two extends of the same person onto the same car, both reading no row,
// with `uq_go_vehicle_shares_accepted_grant` deciding at the INSERT, after the
// audit row is already in the transaction.
//
// Reproduced with a second connection holding an uncommitted duplicate. The
// extend's probe cannot see it (read committed), so it proceeds, writes its
// audit row, and blocks on the index; committing the other transaction turns
// that into the 23505. The test WAITS for the block and fails if it never
// happens, so it cannot silently degrade into testing the probe again.
func TestVehicleShareRepo_ExtendShare_InsertConflictRollsBackTheAudit(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	cleanVehicleShares(t)
	cleanAuditLog(t, testPool)
	sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)

	// A competing accepted grant for the same (grantee, target), held
	// uncommitted so the extend's probe cannot see it.
	blocker, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx,
		`INSERT INTO go_vehicle_shares
		   (id, vehicle_id, owner_user_id, label, permission, allow_rides,
		    accepted_by_user_id, status, created_at, accepted_at, updated_at)
		 VALUES ('cshblocker0000000000000000000001', $1, $2, 'L', 'live', false, $3,
		         'accepted', NOW(), NOW(), NOW())`, vehA2, shareOwnerA, shareViewer1); err != nil {
		t.Fatalf("seed the uncommitted duplicate: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := repo.ExtendShare(ctx, store.ExtendShareInput{
			OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
		})
		done <- err
	}()

	if !waitForBlockedShareInsert(t) {
		t.Fatal("the extend never blocked on the accepted-grant index, so this test did not " +
			"reach the insert-time conflict it exists for")
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}

	if err := <-done; !errors.Is(err, store.ErrShareAlreadyGranted) {
		t.Fatalf("err = %v, want ErrShareAlreadyGranted — a 23505 on the accepted-grant index "+
			"IS the conflict", err)
	}
	if got := countQuery(t, `SELECT count(*) FROM "AuditLog" WHERE "action" = $1`,
		string(store.AuditActionShareExtended)); got != 0 {
		t.Errorf("%d share.extended audit rows survive an insert-time conflict, want 0 — the "+
			"audit row was already written when the INSERT failed, so only the rollback "+
			"can remove it", got)
	}
}

// waitForBlockedShareInsert polls pg_stat_activity until a backend is waiting
// on a lock inside the extend's INSERT, which is the state that proves the
// probe let the call through. Returns false on timeout rather than hanging.
func waitForBlockedShareInsert(t *testing.T) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := testPool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock'
			   AND query LIKE '%INSERT INTO go_vehicle_shares%'`).Scan(&n); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// The PRE-INSERT probe refuses without writing anything at all, which is the
// ordinary path and the one an owner actually hits.
func TestVehicleShareRepo_ExtendShare_ProbeRefusalWritesNoAudit(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	cleanVehicleShares(t)
	cleanAuditLog(t, testPool)
	sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)
	if _, err := repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
	}); err != nil {
		t.Fatalf("seed the first extend: %v", err)
	}

	if _, err := repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
	}); !errors.Is(err, store.ErrShareAlreadyGranted) {
		t.Fatalf("err = %v, want ErrShareAlreadyGranted", err)
	}

	if got := countQuery(t, `SELECT count(*) FROM "AuditLog" WHERE "action" = $1`,
		string(store.AuditActionShareExtended)); got != 1 {
		t.Errorf("%d share.extended audit rows, want exactly 1 — the refused call must have "+
			"written none", got)
	}
}
