// OWNER WINS (MYR-599): what happens when the real owner of a car links their
// Tesla account and finds their own vehicle already claimed by somebody who only
// DRIVES it.
//
// ── THE LOCKOUT THIS FIXES, AND WHY IT WAS UNRECOVERABLE ────────────────────
//
// `"Vehicle"."teslaVehicleId"` and `"Vehicle"."vin"` are UNIQUE. Before MYR-599
// a driver-access car was never provisioned at all, so those unique keys were
// only ever claimed by an owner and the cross-user rule
// (`WHERE "Vehicle"."userId" = EXCLUDED."userId"`) protected exactly the right
// thing: one person could not take another's car.
//
// MYR-599 made a driver's PASSIVE AfterLink sync claim those keys. The
// consequence is not a corner case, it is the ordinary sequence of events for
// this feature: a household shares a car, the driver links first — which is the
// whole scenario the client asked for — and then the OWNER links, their upsert
// conflicts on a row that belongs to the driver, the DO UPDATE predicate fails,
// and they get `VehicleSkippedCrossUser`. Their own car never appears in their
// own app. Nothing in the system can fix it: the tombstone path only clears the
// caller's own tombstones, the re-add path runs the same upsert, and the driver
// has no reason to remove a car that works for them.
//
// ── THE RULE ────────────────────────────────────────────────────────────────
//
// OWNER WINS, BOTH WAYS.
//
//   - An OWNER-access link that conflicts with a DRIVER-PROVISIONED row TAKES
//     the row, in the provisioning transaction, and tears down everything the
//     previous linker had built on it.
//   - A DRIVER-access link that conflicts with a row an OWNER already holds
//     stays `VehicleSkippedCrossUser`, unchanged. The driver is not locked out
//     of anything: the owner can share the car back, which is the documented
//     working path and the one the platform is designed around.
//
// So the unique key always ends up where the stronger claim is, and the weaker
// claim always has a route back that does not require the other party to act
// against their own interest.
//
// ── WHAT THE FORMER DRIVER LOSES, STATED PLAINLY ────────────────────────────
//
// The car leaves their list. Their fleet-config schedule row, their consent
// gate, and every share or invite they issued on the car are gone or revoked.
// That is not collateral damage, it is the point: those are all assertions about
// a car they do not own, and leaving any of them standing would mean the real
// owner inherits a vehicle carrying a stranger's viewers.
//
// It is also why this writes an audit row rather than doing it silently. If the
// former driver asks where their car went, the answer has to exist somewhere.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/myrobotaxi/telemetry/internal/vin"
)

// queryLockConflictingVehicle reads — and ROW-LOCKS — the row the upsert just
// conflicted with, together with whether it carries a driver-access row.
//
// `FOR UPDATE OF v` locks the "Vehicle" row only; the LEFT JOINed Go-owned table
// is read, not locked, which is what a lock clause naming one relation does. The
// lock is what makes the read-then-transfer safe against a concurrent second
// owner link or a teardown landing between them.
//
// The driver-access row's PRESENCE is the whole test. This transfer is not "the
// owner outranks whoever is there" in general — a genuine owner-versus-owner
// conflict on one teslaVehicleId is a data problem the platform must not
// silently resolve by moving a car — it is specifically "the row was provisioned
// by somebody Tesla calls a DRIVER of this car", which is a state only MYR-599
// can create and which an OWNER unambiguously outranks.
const queryLockConflictingVehicle = `
SELECT v."id", v."userId", (dva.vehicle_id IS NOT NULL)
FROM "Vehicle" v
LEFT JOIN go_vehicle_driver_access dva ON dva.vehicle_id = v."id"
WHERE v."teslaVehicleId" = $1
FOR UPDATE OF v`

// queryTransferVehicleToOwner moves the row to its owner.
//
// It reuses the ON CONFLICT arm's exact backfill discipline — `NULLIF`-guarded
// name/model/year — rather than overwriting: a car the driver named, or whose
// richer model string the Prisma web-link flow wrote, keeps what it has. The
// owner is receiving a real row with real history, not a fresh provision, and
// the transfer is about WHOSE it is rather than what it says.
//
// The VIN is refreshed unconditionally, exactly as the upsert does, because the
// VIN is Tesla's answer about this teslaVehicleId and cannot legitimately differ.
const queryTransferVehicleToOwner = `
UPDATE "Vehicle"
SET "userId"    = $2,
    "vin"       = $3,
    "name"      = COALESCE(NULLIF("name", ''), $4),
    "model"     = COALESCE(NULLIF("model", ''), $5),
    "year"      = COALESCE(NULLIF("year", 0), $6),
    "updatedAt" = NOW()
WHERE "id" = $1`

// driverLinkSupersededAuditMetadata is the whole P0 payload of the
// `vehicle.driver_link_superseded_by_owner` row: the id of the account the car
// moved TO, and nothing else (CG-DL-5).
//
// The row's own columns already carry the other two facts — `userId` is the
// FORMER driver (whose data this row is about, matching every other audit row in
// this package) and `targetId` is the car. Two opaque cuids and no third fact:
// no VIN (P1), no access token, no share list.
type driverLinkSupersededAuditMetadata struct {
	OwnerUserID string `json:"ownerUserId"`
}

// resolveCrossUserConflict decides what a cross-user upsert conflict means.
//
// It is called with the transaction still open and nothing written. Every exit
// except the transfer leaves it that way.
func (p *OwnerProvisioner) resolveCrossUserConflict(
	ctx context.Context, tx pgx.Tx, in OwnedVehicleInput, name string,
) (VehicleUpsertResult, error) {
	// ONLY AN EXPLICIT OWNER SIGNAL OUTRANKS ANYTHING. A DRIVER link keeps the
	// old behaviour — the car stays where it is — and so does an UNKNOWN one:
	// moving a vehicle between accounts on the strength of a claim nobody made
	// is precisely the fail-open the tri-state exists to prevent.
	if in.Access != AccessSignalOwner {
		return VehicleUpsertResult{Outcome: VehicleSkippedCrossUser}, nil
	}

	var (
		vehicleID       string
		previousUserID  string
		driverProvision bool
	)
	err := tx.QueryRow(ctx, queryLockConflictingVehicle, in.TeslaVehicleID).
		Scan(&vehicleID, &previousUserID, &driverProvision)
	if errors.Is(err, pgx.ErrNoRows) {
		// The row was deleted between the upsert's conflict and this read — a
		// teardown landing in the gap. Nothing to transfer and nothing to
		// report; the next link provisions it cleanly.
		return VehicleUpsertResult{Outcome: VehicleSkippedCrossUser}, nil
	}
	if err != nil {
		return VehicleUpsertResult{}, fmt.Errorf("store.UpsertOwnedVehicle(user=%s, vin=%s): lock conflicting vehicle: %w",
			in.UserID, redactVIN(in.VIN), err)
	}
	// Not driver-provisioned means an owner-versus-owner collision on one
	// teslaVehicleId. That is a data problem, not a consent one, and moving a
	// car to resolve it would be a guess with somebody's vehicle. Unchanged
	// behaviour: skip, and let the caller's audit line surface it.
	//
	// previousUserID == in.UserID cannot normally reach here (the DO UPDATE
	// predicate would have matched), but it is checked rather than assumed: a
	// "transfer" to the current holder would revoke their own shares.
	if !driverProvision || previousUserID == in.UserID {
		return VehicleUpsertResult{Outcome: VehicleSkippedCrossUser}, nil
	}

	if err := p.transferDriverProvisionedVehicle(ctx, tx, in, name, vehicleID, previousUserID); err != nil {
		return VehicleUpsertResult{}, err
	}
	return VehicleUpsertResult{Outcome: VehicleOwnedByTransfer, VehicleID: vehicleID}, nil
}

// transferDriverProvisionedVehicle hands one driver-provisioned car to its
// owner and dismantles the previous linker's claim on it.
//
// THE ORDER IS NORMATIVE, and it is the audit-first order this package uses
// everywhere (CG-DL-3): the row that explains the change is written before the
// change, so a failure downstream cannot leave a car that moved with nothing
// saying it did.
//
// Everything after that is a teardown of assertions that were only ever true
// about the FORMER holder:
//
//   - the CONSENT GATE. It says "this car is driver-linked and its gate is
//     open/shut", which is a statement about a relationship that no longer
//     governs the row. Left standing on an owner's car it would keep the wire
//     saying `teslaAccessType: "driver"` about a car they own outright and —
//     if unacknowledged — hold every push path shut against them.
//   - the FLEET-CONFIG SCHEDULE. Its attempt count, its backoff and its
//     `awaiting_owner_ack` label were all earned under an authorization that is
//     gone. The owner deserves a fresh count, and a surviving `awaiting_owner_ack`
//     would additionally exempt their car from the MYR-592 sweeper forever.
//   - the SHARES AND INVITES. REVOKED rather than deleted, through the same
//     statement the per-vehicle teardown uses, so the audit trail of who could
//     see this car survives the change of hands. This is the one that would
//     actually hurt if it were forgotten: the new owner would inherit a car
//     that a stranger's contacts can watch, with no UI anywhere that would ever
//     show them why.
//
// The suspension episode is deliberately NOT cleared: it is a fact about the
// CAR's Tesla-side config being removed for inactivity, which stays true across
// a change of holder, and the owner's own §7.28 reconnect is the right way out
// of it.
func (p *OwnerProvisioner) transferDriverProvisionedVehicle(
	ctx context.Context, tx pgx.Tx, in OwnedVehicleInput, name, vehicleID, previousUserID string,
) error {
	if err := insertDriverLinkSupersededAudit(ctx, tx, previousUserID, vehicleID, in.UserID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, queryTransferVehicleToOwner,
		vehicleID, in.UserID, in.VIN, name, vin.Model(in.VIN), vin.ModelYear(in.VIN)); err != nil {
		return fmt.Errorf("store.UpsertOwnedVehicle(user=%s): transfer vehicle: %w", in.UserID, err)
	}
	if _, err := tx.Exec(ctx, queryDeleteDriverAccessByVehicle, vehicleID); err != nil {
		return fmt.Errorf("store.UpsertOwnedVehicle(user=%s): clear superseded driver access: %w", in.UserID, err)
	}
	if _, err := tx.Exec(ctx, queryTeardownDeleteFleetConfigAttempts, vehicleID); err != nil {
		return fmt.Errorf("store.UpsertOwnedVehicle(user=%s): clear superseded fleet-config schedule: %w", in.UserID, err)
	}
	if _, err := tx.Exec(ctx, queryRevokeSharesForVehicle, vehicleID); err != nil {
		return fmt.Errorf("store.UpsertOwnedVehicle(user=%s): revoke superseded shares: %w", in.UserID, err)
	}
	return nil
}

// insertDriverLinkSupersededAudit writes the `vehicle.driver_link_superseded_by_owner`
// row inside the provisioning transaction.
//
// `userId` is the FORMER DRIVER, not the arriving owner. Audit rows in this
// package name the person whose data the action was about, and the data that
// changed here is theirs: a car left their list and their shares were revoked.
// The owner's id rides in the metadata so the two ends of the move are joinable
// from either side.
//
// Reuses the same-package queryAuditInsert column list — the single source of
// truth shared with AuditRepo, which is what keeps CG-DL-8 column parity
// automatic.
func insertDriverLinkSupersededAudit(ctx context.Context, tx pgx.Tx, previousUserID, vehicleID, ownerUserID string) error {
	meta, err := json.Marshal(driverLinkSupersededAuditMetadata{OwnerUserID: ownerUserID})
	if err != nil {
		return fmt.Errorf("store.UpsertOwnedVehicle: marshal transfer audit metadata: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, queryAuditInsert,
		newProvisionID(), // id (cuid)
		previousUserID,   // userId — the driver whose claim was superseded
		now,              // timestamp
		string(AuditActionDriverLinkSupersededByOwner), // action
		auditTargetTypeVehicle,                         // targetType
		vehicleID,                                      // targetId
		auditInitiatorSystemProvisioner,                // initiator
		meta,                                           // metadata (P0: one opaque cuid)
		now,                                            // createdAt
	); err != nil {
		return fmt.Errorf("store.UpsertOwnedVehicle: insert transfer audit: %w", err)
	}
	return nil
}
