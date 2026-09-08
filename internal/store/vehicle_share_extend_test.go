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
	// The code column is NOT NULL in the schema, so the row has one — and the
	// projection must never let it out. The value itself is P1 and is not
	// reported here even on failure.
	if row.Code != "" {
		t.Error("an accepted row carried a `code` out of the repository; the SQL projection " +
			"blanks it for exactly this reason")
	}

	t.Run("the source grant is untouched", func(t *testing.T) {
		again := extendedGrantOn(t, repo, vehA1)
		if again.ID != sourceID || again.Status != store.ShareStatusAccepted {
			t.Errorf("source row changed: id=%s status=%s", again.ID, again.Status)
		}
	})
}

// A SUSPENDED source copies its pause forward. Refusing to extend a suspended
// grant would force an owner to un-pause somebody in order to add them to a
// second car, which is the opposite of what a pause means; copying it LIVE
// would silently undo the pause on the new car.
func TestVehicleShareRepo_ExtendShare_CopiesSuspension(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	ensureAuditSchema(t)
	repo := newShareRepo(t)

	cleanVehicleShares(t)
	sourceID := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionRides)
	if _, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
		InviteID: sourceID, OwnerUserID: shareOwnerA, Suspended: boolPtr(true),
	}); err != nil {
		t.Fatalf("suspend source: %v", err)
	}

	row, err := repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID:     shareOwnerA,
		TargetVehicleID: vehA2,
		SourceShareID:   sourceID,
	})
	if err != nil {
		t.Fatalf("ExtendShare from a suspended source: %v — a pause is the owner's own "+
			"reversible state and must not block them adding a car", err)
	}
	if row.SuspendedAt == nil {
		t.Fatal("the extended grant is LIVE though the source was suspended; extending would " +
			"otherwise be a way to undo a pause without lifting it")
	}

	// And the suspension invariant holds on the new row: a suspended grant is
	// excluded from the access set, so the target car must not appear.
	if ids := authAccessSet(t, shareViewer1); slices.Contains(ids, vehA2) {
		t.Errorf("access set %v contains %s; a suspended grant conveys nothing", ids, vehA2)
	}
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

	t.Run("a malformed input is refused before any write", func(t *testing.T) {
		for _, in := range []store.ExtendShareInput{
			{TargetVehicleID: vehA2, SourceShareID: "x"},
			{OwnerUserID: shareOwnerA, SourceShareID: "x"},
			{OwnerUserID: shareOwnerA, TargetVehicleID: vehA2},
		} {
			if _, err := repo.ExtendShare(ctx, in); err == nil {
				t.Errorf("ExtendShare(%+v) succeeded; an incomplete input must be refused", in)
			}
		}
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
func TestVehicleShareRepo_ExtendShare_RefusalWritesNoAudit(t *testing.T) {
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

	// The second extend conflicts, after the audit INSERT would have run had it
	// been ordered outside the transaction.
	if _, err := repo.ExtendShare(ctx, store.ExtendShareInput{
		OwnerUserID: shareOwnerA, TargetVehicleID: vehA2, SourceShareID: sourceID,
	}); !errors.Is(err, store.ErrShareAlreadyGranted) {
		t.Fatalf("err = %v, want ErrShareAlreadyGranted", err)
	}

	if got := countQuery(t, `SELECT count(*) FROM "AuditLog" WHERE "action" = $1`,
		string(store.AuditActionShareExtended)); got != 1 {
		t.Errorf("%d share.extended audit rows, want exactly 1 — the refused call must have "+
			"rolled its own row back", got)
	}
}
