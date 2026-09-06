package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/myrobotaxi/telemetry/internal/vin"
)

// OwnedVehicleInput seeds the identity columns of a "Vehicle" row for a
// freshly linked owner (MYR-257). Live values (charge, GPS, status) are NOT
// set here — the streaming telemetry pipeline fills them once the car connects.
type OwnedVehicleInput struct {
	UserID         string
	TeslaVehicleID string
	VIN            string
	Name           string
	// TeslaAccessType is Tesla's `access_type` for this vehicle under the
	// linking account, VERBATIM — "OWNER", "DRIVER", or the empty string older
	// Fleet responses have shipped (MYR-599).
	TeslaAccessType string
	// IsOwnerAccess is whether that access type means OWNER.
	//
	// PASSED IN RATHER THAN RE-DERIVED HERE, deliberately: the caller already
	// owns the interpretation (telemetry.FleetVehicle.IsOwner, which treats an
	// empty access_type as NOT owner — fail closed), and re-deriving a
	// fail-closed rule in a second place is how two spellings eventually
	// disagree. Here that disagreement would open a consent gate.
	IsOwnerAccess bool
}

// VehicleUpsertOutcome classifies a vehicle-provision attempt (log-safe, P0).
type VehicleUpsertOutcome string

const (
	// VehicleOwned: the row was inserted or reconciled for this owner.
	VehicleOwned VehicleUpsertOutcome = "owned"
	// VehicleSkippedCrossUser: the teslaVehicleId already belongs to a DIFFERENT
	// user; the row was left untouched (never reassigned) and the caller audits.
	VehicleSkippedCrossUser VehicleUpsertOutcome = "skipped_cross_user"
	// VehicleSkippedTombstoned: the owner deliberately removed this teslaVehicleId
	// (a go_removed_vehicles tombstone exists, MYR-261). The upsert is skipped so a
	// passive Tesla re-link can NOT resurrect a removed car. Cleared only by a
	// deliberate re-add (RemovedVehicleRegistry.ClearTombstone).
	VehicleSkippedTombstoned VehicleUpsertOutcome = "skipped_tombstoned"
)

// queryUpsertOwnedVehicle inserts the minimal identity columns for a newly
// linked vehicle, keyed on the unique "teslaVehicleId". On a same-user conflict
// it refreshes vin/name only (never clobbering live telemetry columns —
// charge/GPS/status — written by the streaming pipeline). The
// `WHERE "Vehicle"."userId" = EXCLUDED."userId"` predicate on the DO UPDATE means
// a conflict against a row owned by a DIFFERENT user updates nothing and reports
// RowsAffected()==0 — the teslaVehicleId is NEVER reassigned across users.
//
// MYR-507: `model` and `year` are no longer seeded with placeholders. That
// comment used to read "the web sync / streaming pipeline fills real values
// later" — and no such writer has ever existed on either side, so for every car
// the GO server provisioned they stayed `”` and `0` permanently. Both are
// `required` wire fields on §7.0 and §7.1, so a rider had no way to name a
// shared car beyond its colour. They are now derived from the VIN
// (`internal/vin`) and passed in as binds, which is possible precisely because
// the VIN needs no token, no awake car and no network call: a car is correctly
// identified from the instant it is linked, rather than waiting on a
// connectivity edge it may never produce.
//
// The ON CONFLICT arm BACKFILLS both — `NULLIF`-guarded exactly like `name`
// above, so a re-link fills an empty column but never overwrites a populated
// one. The Prisma web-link flow writes a richer model for some rows ("Model 3
// Performance") than position 4 of a VIN can encode, and a re-link must not
// downgrade it. (`VehicleRepo.FillVehicleIdentity` applies the same rule on the
// refresh path for cars already in the table.)
//
// `color` and `licensePlate` keep their empty placeholders: neither is
// derivable from the VIN. `color` is filled by the MYR-320 Tesla read;
// `licensePlate` only ever comes from the owner typing it (§7.14).
//
// `xmax = 0` is Postgres's "row was inserted (not updated) by this statement"
// test, used to distinguish an insert from a same-user reconcile.
const queryUpsertOwnedVehicle = `
INSERT INTO "Vehicle" ("id", "userId", "teslaVehicleId", "vin", "name",
                       "model", "year", "color", "licensePlate", "updatedAt")
VALUES ($1, $2, $3, $4, $5, $6, $7, '', '', NOW())
ON CONFLICT ("teslaVehicleId") DO UPDATE
SET "vin"       = EXCLUDED."vin",
    "name"      = COALESCE(NULLIF("Vehicle"."name", ''), EXCLUDED."name"),
    "model"     = COALESCE(NULLIF("Vehicle"."model", ''), EXCLUDED."model"),
    "year"      = COALESCE(NULLIF("Vehicle"."year", 0), EXCLUDED."year"),
    "updatedAt" = NOW()
WHERE "Vehicle"."userId" = EXCLUDED."userId"
RETURNING "id"`

// UpsertOwnedVehicle seeds (or reconciles) a "Vehicle" identity row for a linked
// owner. Idempotent on "teslaVehicleId". Returns VehicleSkippedTombstoned (and no
// error) when the owner deliberately removed this teslaVehicleId (MYR-261 — the
// tombstone gate that stops a passive re-link from resurrecting a removed car),
// and VehicleSkippedCrossUser when the teslaVehicleId is already owned by a
// different user — the row is never reassigned; the caller emits an audit line.
func (p *OwnerProvisioner) UpsertOwnedVehicle(ctx context.Context, in OwnedVehicleInput) (VehicleUpsertOutcome, error) {
	if strings.TrimSpace(in.UserID) == "" {
		return "", fmt.Errorf("store.UpsertOwnedVehicle: empty user id")
	}
	if strings.TrimSpace(in.TeslaVehicleID) == "" {
		return "", fmt.Errorf("store.UpsertOwnedVehicle(user=%s): empty teslaVehicleId", in.UserID)
	}
	// Removed-vehicle tombstone gate (MYR-261): if the owner deliberately removed
	// this teslaVehicleId, SKIP the upsert. This is the fix for the reappearance
	// bug — the teardown leaves Tesla access intact, so without this gate the
	// best-effort AfterLink sync would re-INSERT the still-owned VIN. The check
	// lives here so it covers EVERY re-add sync route through UpsertOwnedVehicle,
	// not just AfterLink. A deliberate re-add clears the tombstone first.
	tombstoned, err := isVehicleTombstoned(ctx, p.pool, in.UserID, in.TeslaVehicleID)
	if err != nil {
		return "", fmt.Errorf("store.UpsertOwnedVehicle(user=%s, vin=%s): %w", in.UserID, redactVIN(in.VIN), err)
	}
	if tombstoned {
		return VehicleSkippedTombstoned, nil
	}

	name := in.Name
	if strings.TrimSpace(name) == "" {
		name = "Tesla"
	}
	// MYR-507: the identity a VIN can be read for without asking Tesla anything.
	// Either may be the zero value for an unrecognised code, which the INSERT
	// stores as the same placeholder this statement used to hard-code and which
	// the refresh path (FillVehicleIdentity) will retry against later.
	// ONE TRANSACTION, because MYR-599 made the second write a CONSENT GATE.
	//
	// The vehicle row and the driver-access row used to be two round trips from
	// the link-time hook, both best-effort. That is the right contract for the
	// vehicle row — a failure costs the owner nothing but a retry — and the
	// wrong one for the gate: a car provisioned WITHOUT its driver-access row is
	// indistinguishable from an owner's, so the reconciler configures it on the
	// next pass, unattended, at a vehicle whose owner never approved anything.
	// The failure that matters reaches a THIRD PARTY, so it cannot be a
	// best-effort step that logs and shrugs.
	//
	// Atomic now: either the car exists WITH its gate, or neither exists and the
	// next link retries both.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store.UpsertOwnedVehicle(user=%s): begin: %w", in.UserID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var vehicleID string
	err = tx.QueryRow(ctx, queryUpsertOwnedVehicle,
		newProvisionID(), in.UserID, in.TeslaVehicleID, in.VIN, name,
		vin.Model(in.VIN), vin.ModelYear(in.VIN)).Scan(&vehicleID)
	// A same-teslaVehicleId row owned by another user fails the DO UPDATE WHERE
	// predicate → no row returned → cross-user skip (never a reassignment).
	// Nothing was written, so there is nothing to commit and no gate to record.
	if errors.Is(err, pgx.ErrNoRows) {
		return VehicleSkippedCrossUser, nil
	}
	if err != nil {
		return "", fmt.Errorf("store.UpsertOwnedVehicle(user=%s, vin=%s): %w", in.UserID, redactVIN(in.VIN), err)
	}

	if err := applyDriverAccess(ctx, tx, vehicleID, in); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store.UpsertOwnedVehicle(user=%s, vin=%s): commit: %w",
			in.UserID, redactVIN(in.VIN), err)
	}
	return VehicleOwned, nil
}

// applyDriverAccess records or retires this car's driver-access row inside the
// provisioning transaction (MYR-599).
//
// TWO DIRECTIONS, and both are load-bearing:
//
//   - NOT OWNER → upsert the row, carrying Tesla's access_type verbatim. This is
//     the gate. It must exist before anything can look at the car.
//   - OWNER → delete any row that is there. The access-UPGRADE case: a title
//     transfer, or an owner who had been reaching their own car through a second
//     account. A stale row would keep the wire saying `teslaAccessType:
//     "driver"` about a car this person owns outright and, if never
//     acknowledged, would hold the push gate shut on a car needing nobody's
//     permission.
//
// The upsert deliberately does NOT touch acknowledged_at or created_at: a
// re-link must refresh what Tesla currently says without re-shutting a gate the
// person already opened, or every incidental re-link would demand a second
// acknowledgment for a car that is already streaming. Consent, once given, is
// not withdrawn by a background sync.
func applyDriverAccess(ctx context.Context, tx pgx.Tx, vehicleID string, in OwnedVehicleInput) error {
	if in.IsOwnerAccess {
		if _, err := tx.Exec(ctx, queryDeleteDriverAccessByVehicle, vehicleID); err != nil {
			return fmt.Errorf("store.UpsertOwnedVehicle(user=%s): clear driver access: %w", in.UserID, err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, queryUpsertDriverAccessByVehicle,
		vehicleID, in.UserID, in.TeslaAccessType); err != nil {
		return fmt.Errorf("store.UpsertOwnedVehicle(user=%s): record driver access: %w", in.UserID, err)
	}
	return nil
}
