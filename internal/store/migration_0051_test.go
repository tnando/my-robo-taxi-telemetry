package store_test

import (
	"context"
	"testing"
	"time"
)

// MYR-609 migrations 0051 and 0052 — the two schema changes §7.5.8 extend
// needed: a tombstone that names its AUTHOR, and a credential that is allowed
// to be absent.

// migration0051Columns is the shape 0051 ADDS. It joins the union counted by
// the undocumented-column guard in migration_0020_test.go.
//
// BOTH NULLABLE, and NULL means different things on each. On `revoked_by` it
// means "the author was never recorded" — every tombstone predating the
// migration — which is why the extend gate fails OPEN on it. On
// `revoked_reason` it is the ordinary state: only a `superseded` tombstone has
// anything to explain beyond who wrote it.
var migration0051Columns = map[string]string{
	"revoked_by":     "text",
	"revoked_reason": "text",
}

// TestMigration0051_ConstrainsTheTombstoneAuthor pins the CHECK. The extend
// gate BRANCHES on this value, so an unconstrained column would be a third
// state nobody decided — and the branch would silently take the fail-open arm
// for it, which is the wrong direction for a typo'd 'grantee'.
func TestMigration0051_ConstrainsTheTombstoneAuthor(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanVehicleShares(t)
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)

	for _, tt := range []struct {
		name    string
		author  any
		wantErr bool
	}{
		{"owner", "owner", false},
		{"grantee", "grantee", false},
		{"NULL — the pre-0051 tail, and the fail-open arm", nil, false},
		{"anything else", "admin", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := testPool.Exec(ctx,
				`INSERT INTO go_vehicle_shares
				   (id, vehicle_id, owner_user_id, label, permission, code, status,
				    expires_at, revoked_at, revoked_by)
				 VALUES ($1, 'veh-0051', 'own1', 'L', 'live', $2, 'revoked', $3, NOW(), $4)`,
				"csh0051"+tt.name[:1], "C"+tt.name[:5], future, tt.author)
			if tt.wantErr && err == nil {
				t.Fatalf("revoked_by = %v was accepted, want a CHECK violation", tt.author)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("revoked_by = %v was refused: %v", tt.author, err)
			}
		})
	}
}

// TestMigration0052_RequiresACredentialOnlyWhilePending pins the implication
// that replaced two NOT NULLs.
//
// It is the whole point of the migration: an extended grant is born `accepted`
// and stores neither a code nor an expiry, while a PENDING row without both
// would be an invite nobody can redeem — a shape the old NOT NULLs happened to
// forbid and this CHECK forbids on purpose.
func TestMigration0052_RequiresACredentialOnlyWhilePending(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanVehicleShares(t)
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)
	viewer := "viewer-0052"

	t.Run("an ACCEPTED row may carry neither", func(t *testing.T) {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_vehicle_shares
			   (id, vehicle_id, owner_user_id, label, permission, status, accepted_by_user_id, accepted_at)
			 VALUES ('csh0052a', 'veh-0052a', 'own1', 'L', 'live', 'accepted', $1, NOW())`, viewer); err != nil {
			t.Fatalf("accepted row with NULL code and NULL expires_at was refused: %v — this is "+
				"exactly the row §7.5.8 extend writes", err)
		}
	})

	t.Run("a PENDING row must carry both", func(t *testing.T) {
		for _, tt := range []struct {
			name         string
			code, expiry any
		}{
			{"no code", nil, future},
			{"no expiry", "AAAAAA", nil},
			{"neither", nil, nil},
		} {
			t.Run(tt.name, func(t *testing.T) {
				_, err := testPool.Exec(ctx,
					`INSERT INTO go_vehicle_shares
					   (id, vehicle_id, owner_user_id, label, permission, status, code, expires_at)
					 VALUES ($1, 'veh-0052b', 'own1', 'L', 'live', 'pending', $2, $3)`,
					"csh0052"+tt.name[3:6], tt.code, tt.expiry)
				if err == nil {
					t.Fatalf("a pending row with %s was accepted; nobody could ever redeem it", tt.name)
				}
			})
		}
	})
}
