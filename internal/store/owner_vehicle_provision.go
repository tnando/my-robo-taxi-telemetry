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
	// Access is the caller's INTERPRETATION of that access type, as an explicit
	// three-valued signal whose ZERO VALUE IS "unknown, do not touch the gates"
	// (owner_vehicle_access_signal.go).
	//
	// It replaced an `IsOwnerAccess bool` whose zero value meant DRIVER. That
	// bool was fail-closed at the boundary and fail-DANGEROUS as a Go default:
	// a hand-built input, a fixture, or a future caller that simply forgot the
	// field asked this transaction to gate the car — and on an established
	// owner's streaming vehicle "gate it" is not the safe direction, it is a
	// customer's car going dark behind a sheet asking them to confirm somebody
	// else's approval.
	//
	// Build it with AccessSignalFor, which holds the ONE spelling of the
	// fail-closed rule (anything that is not OWNER, empty included, is DRIVER).
	Access TeslaAccessSignal
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
	// VehicleOwnedByTransfer: the teslaVehicleId belonged to a DIFFERENT user
	// who holds it under DRIVER access, and this link is the car's real OWNER —
	// so the row was transferred to them, in this transaction, along with the
	// teardown of everything the previous linker had built on it (MYR-599).
	// OWNER WINS. See owner_vehicle_transfer.go for the whole argument.
	VehicleOwnedByTransfer VehicleUpsertOutcome = "owned_by_transfer"
)

// VehicleUpsertResult is what one provisioning attempt actually did.
//
// IT REPLACED A BARE VehicleUpsertOutcome because three separate MYR-599 rules
// need a fact the outcome alone cannot carry, and each of them was being guessed
// at the call site instead:
//
//   - the link-time hook must SEED `awaiting_owner_ack` only for a car whose
//     gate is actually shut. It used to seed on "Tesla called this person a
//     driver", so every pass over an ALREADY-ACKNOWLEDGED streaming driver car
//     re-inserted the label — and the sweeper, which exempts that label,
//     exempted the car forever;
//   - the same hook must PUSH for an acknowledged driver car, which is the same
//     question from the other side;
//   - the downgrade REFUSAL (an established owner row, a non-OWNER signal) has
//     to be logged by somebody, and the store is not where this codebase logs.
type VehicleUpsertResult struct {
	// Outcome classifies what happened to the "Vehicle" row.
	Outcome VehicleUpsertOutcome
	// VehicleID is the row's cuid. Empty on either skip outcome.
	VehicleID string
	// Inserted reports that this statement CREATED the row rather than
	// reconciling one that already existed (Postgres's `xmax = 0`). It is what
	// separates "a new car arriving with an unknown access level" — gate it,
	// fail closed — from "a car we have had all along" — never gate it from a
	// signal that is merely not OWNER.
	Inserted bool
	// DriverAccessPresent reports that the car carries a driver-access row after
	// this write. FALSE for an owner's car, and false when the downgrade refusal
	// below fired.
	DriverAccessPresent bool
	// DriverAccessPending reports that the row is present AND unacknowledged —
	// the shut gate. The ONE question every push path asks, answered here for
	// the one caller that holds no vehicle read of its own.
	DriverAccessPending bool
	// AccessDowngradeObserved reports that a NON-OWNER signal arrived for an
	// ESTABLISHED row that carries no driver-access row, and was REFUSED: no
	// gate was written. It is not an error and not a failure — it is a fact
	// about Tesla's answer that the caller should say out loud, because the
	// alternative reading (a real access downgrade, which MYR-599 explicitly
	// does not handle) would matter if it ever became common.
	AccessDowngradeObserved bool
	// PreviousUserID is the account the car was taken FROM on the
	// VehicleOwnedByTransfer path, and empty on every other outcome (MYR-601).
	//
	// IT IS HERE BECAUSE A TRANSFER IS TWO ACCESS-SET CHANGES, NOT ONE. The
	// arriving owner GAINS the car and the former driver LOSES it, in the same
	// statement — and until MYR-601 the caller could only see the first half.
	// The former driver's cached access set stayed warm for the TTL and their
	// already-open WebSocket kept the car's live GPS flowing to somebody the
	// row no longer says anything about, which is the narrowing direction and
	// therefore a security property rather than a convenience.
	//
	// The audit row inside the transaction names the same account, so this is
	// not a new fact — it is the one fact the transaction already recorded,
	// handed to the caller that has to act on it.
	PreviousUserID string
}

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
RETURNING "id", (xmax = 0)`

// UpsertOwnedVehicle seeds (or reconciles) a "Vehicle" identity row for a linked
// owner. Idempotent on "teslaVehicleId".
//
// Returns VehicleSkippedTombstoned (and no error) when the owner deliberately
// removed this teslaVehicleId (MYR-261 — the tombstone gate that stops a passive
// re-link from resurrecting a removed car), and VehicleSkippedCrossUser when the
// teslaVehicleId is already held by a different user under an access level this
// link does not outrank; the caller emits an audit line.
//
// THE ONE CROSS-USER CONFLICT THAT IS *NOT* A SKIP is an OWNER arriving at a car
// a DRIVER provisioned. That is VehicleOwnedByTransfer, and it is handled in
// this same transaction — see transferDriverProvisionedVehicle.
func (p *OwnerProvisioner) UpsertOwnedVehicle(ctx context.Context, in OwnedVehicleInput) (VehicleUpsertResult, error) {
	if strings.TrimSpace(in.UserID) == "" {
		return VehicleUpsertResult{}, fmt.Errorf("store.UpsertOwnedVehicle: empty user id")
	}
	if strings.TrimSpace(in.TeslaVehicleID) == "" {
		return VehicleUpsertResult{}, fmt.Errorf("store.UpsertOwnedVehicle(user=%s): empty teslaVehicleId", in.UserID)
	}
	// Removed-vehicle tombstone gate (MYR-261): if the owner deliberately removed
	// this teslaVehicleId, SKIP the upsert. This is the fix for the reappearance
	// bug — the teardown leaves Tesla access intact, so without this gate the
	// best-effort AfterLink sync would re-INSERT the still-owned VIN. The check
	// lives here so it covers EVERY re-add sync route through UpsertOwnedVehicle,
	// not just AfterLink. A deliberate re-add clears the tombstone first.
	//
	// It also guards the MYR-599 TRANSFER below: a car this owner deliberately
	// removed is never taken back from a driver by a passive re-link either.
	tombstoned, err := isVehicleTombstoned(ctx, p.pool, in.UserID, in.TeslaVehicleID)
	if err != nil {
		return VehicleUpsertResult{}, fmt.Errorf("store.UpsertOwnedVehicle(user=%s, vin=%s): %w",
			in.UserID, redactVIN(in.VIN), err)
	}
	if tombstoned {
		return VehicleUpsertResult{Outcome: VehicleSkippedTombstoned}, nil
	}

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
	// next link retries both. The MYR-599 transfer joined the same transaction
	// for the mirror-image reason — a car handed to its owner while the previous
	// linker's gate, schedule and shares survived would be worse than either
	// end state on its own.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return VehicleUpsertResult{}, fmt.Errorf("store.UpsertOwnedVehicle(user=%s): begin: %w", in.UserID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	res, err := p.upsertInTx(ctx, tx, in)
	if err != nil {
		return VehicleUpsertResult{}, err
	}
	if res.Outcome == VehicleSkippedCrossUser {
		// Nothing was written, so there is nothing to commit.
		return res, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return VehicleUpsertResult{}, fmt.Errorf("store.UpsertOwnedVehicle(user=%s, vin=%s): commit: %w",
			in.UserID, redactVIN(in.VIN), err)
	}
	return res, nil
}

// upsertInTx is the whole provisioning decision, inside the caller's open
// transaction: write (or reconcile) the identity row, take the MYR-599 transfer
// exit when the conflict is one an OWNER outranks, and settle the consent gate.
func (p *OwnerProvisioner) upsertInTx(
	ctx context.Context, tx pgx.Tx, in OwnedVehicleInput,
) (VehicleUpsertResult, error) {
	name := in.Name
	if strings.TrimSpace(name) == "" {
		name = "Tesla"
	}

	// MYR-507: the identity a VIN can be read for without asking Tesla anything.
	// Either may be the zero value for an unrecognised code, which the INSERT
	// stores as the same placeholder this statement used to hard-code and which
	// the refresh path (FillVehicleIdentity) will retry against later.
	var (
		vehicleID string
		inserted  bool
	)
	err := tx.QueryRow(ctx, queryUpsertOwnedVehicle,
		newProvisionID(), in.UserID, in.TeslaVehicleID, in.VIN, name,
		vin.Model(in.VIN), vin.ModelYear(in.VIN)).Scan(&vehicleID, &inserted)
	// A same-teslaVehicleId row held by another user fails the DO UPDATE WHERE
	// predicate → no row returned. Before MYR-599 that was always a skip; now it
	// is the door the owner-wins transfer opens, and only for an OWNER link.
	if errors.Is(err, pgx.ErrNoRows) {
		return p.resolveCrossUserConflict(ctx, tx, in, name)
	}
	if err != nil {
		return VehicleUpsertResult{}, fmt.Errorf("store.UpsertOwnedVehicle(user=%s, vin=%s): %w",
			in.UserID, redactVIN(in.VIN), err)
	}

	res := VehicleUpsertResult{Outcome: VehicleOwned, VehicleID: vehicleID, Inserted: inserted}
	if err := applyDriverAccess(ctx, tx, &res, in); err != nil {
		return VehicleUpsertResult{}, err
	}
	return res, nil
}
