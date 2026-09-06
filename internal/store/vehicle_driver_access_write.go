// The WRITE half of MYR-599: recording that a car was linked by a driver,
// removing that record when the same person turns out to own the car after all,
// and stamping the acknowledgment that opens the config-push gate.
//
// Split from vehicle_driver_access.go for the CLAUDE.md 300-line file cap; the
// read projection and these statements are one component.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// queryUpsertDriverAccessByVIN records (or refreshes) the driver-access row for
// the vehicle owning vin, scoped to the linking user.
//
// KEYED BY VIN THROUGH A SELECT ON "Vehicle" rather than by vehicle id, for the
// same reason SeedFleetConfigSchedule is: the caller is the link-time hook,
// which holds Tesla's VIN and not our cuid, and resolving it in SQL keeps this
// one statement instead of a lookup plus an insert that could race the
// provisioning INSERT that just ran.
//
// THE `"userId" = $2` PREDICATE IS A GUARD, NOT A CONVENIENCE. UpsertOwnedVehicle
// refuses to reassign a car that already belongs to somebody else (the
// cross-user rule, MYR-257) and returns VehicleSkippedCrossUser — but this
// statement runs after a SEPARATE round trip, so a car that changed hands in
// between must not have a driver row filed against the wrong person. Matching on
// both columns makes that outcome unreachable: the statement simply writes
// nothing.
//
// ON CONFLICT UPDATES THE ACCESS TYPE AND NOTHING ELSE. A re-link must refresh
// what Tesla currently says (a driver whose access level Tesla re-labelled),
// but it must NOT re-shut a gate the person already opened: clobbering
// acknowledged_at would make every incidental re-link demand a second
// acknowledgment for a car that is already streaming, and clobbering created_at
// would restate the `since` on a state nobody is in. Consent, once given, is not
// withdrawn by a background sync.
//
// A READ of the Prisma-owned "Vehicle" feeding an INSERT into a Go-owned table.
// CG-DL-9 constrains MIGRATIONS naming Prisma tables; this is a runtime
// statement and adds no schema.
const queryUpsertDriverAccessByVIN = `
INSERT INTO go_vehicle_driver_access (vehicle_id, user_id, tesla_access_type, created_at)
SELECT v."id", $2, $3, $4
FROM "Vehicle" v
WHERE v."vin" = $1 AND v."userId" = $2
ON CONFLICT (vehicle_id) DO UPDATE
SET tesla_access_type = EXCLUDED.tesla_access_type`

// RecordDriverAccess files the driver-access row for vin against userID,
// carrying Tesla's access_type verbatim.
//
// accessType IS STORED AS GIVEN, including the empty string. Older Fleet API
// responses have shipped an absent access_type, and the caller treats absence as
// NOT-OWNER (fail closed — an unknown access level must never be promoted to
// ownership). Inventing a value here would erase the one thing this column is
// for: answering, later, what Tesla actually said.
//
// Best-effort by contract, like every other step in the link-time hook: an
// unknown VIN or a car that changed hands writes nothing and is success. The
// caller logs a returned error and never fails the owner's Tesla link over it.
//
// IDEMPOTENT: a re-link refreshes the access type and leaves the acknowledgment
// exactly as it was — see queryUpsertDriverAccessByVIN.
func (p *OwnerProvisioner) RecordDriverAccess(
	ctx context.Context, vin, userID, accessType string, now time.Time,
) error {
	if strings.TrimSpace(vin) == "" || strings.TrimSpace(userID) == "" {
		return fmt.Errorf("store.RecordDriverAccess: empty vin or user id")
	}
	if _, err := p.pool.Exec(ctx, queryUpsertDriverAccessByVIN, vin, userID, accessType, now); err != nil {
		return fmt.Errorf("OwnerProvisioner.RecordDriverAccess: %w", err)
	}
	return nil
}

// queryDeleteDriverAccessByVIN removes the driver-access row for the vehicle
// owning vin, scoped to the same user for the same reason the upsert is.
const queryDeleteDriverAccessByVIN = `
DELETE FROM go_vehicle_driver_access
WHERE vehicle_id IN (
    SELECT v."id" FROM "Vehicle" v WHERE v."vin" = $1 AND v."userId" = $2
)`

// ClearDriverAccess removes the driver-access row for vin.
//
// CALLED WHEN AN OWNER-ACCESS LISTING ARRIVES for a car that carries one, which
// is the access-UPGRADE case: the person was a driver when they first linked and
// Tesla now reports them as the OWNER (a title transfer, or an owner who had
// been sharing their own car back to themselves through a second account). The
// row is EVIDENCE ABOUT A CLAIM THAT IS NO LONGER TRUE, and a stale one would
// keep the wire saying `teslaAccessType: "driver"` about a car this person owns
// outright — and, if it were never acknowledged, would keep the push gate shut
// on a car nobody needs permission for.
//
// It does NOT run the other way. Tesla downgrading an owner to a driver is not
// observed here (nothing re-lists an already-provisioned owner's cars for that
// purpose), and the gap is recorded in the PR rather than papered over.
//
// Idempotent: deleting zero rows is the ordinary result and is success.
func (p *OwnerProvisioner) ClearDriverAccess(ctx context.Context, vin, userID string) error {
	if strings.TrimSpace(vin) == "" || strings.TrimSpace(userID) == "" {
		return fmt.Errorf("store.ClearDriverAccess: empty vin or user id")
	}
	if _, err := p.pool.Exec(ctx, queryDeleteDriverAccessByVIN, vin, userID); err != nil {
		return fmt.Errorf("OwnerProvisioner.ClearDriverAccess: %w", err)
	}
	return nil
}

// queryAcknowledgeDriverAccess stamps the acknowledgment on one car's row.
//
// FIRST WRITE WINS — that is what `AND acknowledged_at IS NULL` is for, and it
// is the opposite of the usual last-write-wins upsert in this package.
//
// The row is a CONSENT RECORD, and the instant a person first agreed is the
// thing the platform would point to if an owner ever complained. A later call
// must not be able to move it: a client that lost a response and retried, a
// background re-sync, or a second tap would otherwise quietly restate a
// months-old agreement as today's, and the one fact worth having would be the
// one fact that drifts. Consent, once given, is not re-dated.
//
// It follows that this statement is SELF-DEBOUNCING, and the handler leans on
// that: matching zero rows means "nothing to record here", which covers the
// OWNER-ACCESS car (no row at all) and the ALREADY-ACKNOWLEDGED one (a row the
// predicate excludes) — and in both cases the right behaviour is identical: no
// stamp, no audit row, 200, and the ordinary setup state. The endpoint stays
// idempotent because the PUSH still runs on every call; only the record is
// written once.
//
// A driver who is later shown a NEWER version of the copy therefore does not
// overwrite this row. The append-only AuditLog trail is where a second
// acknowledgment would be recorded, and it is the only surface that can hold
// more than one — which is the right shape for a history.
// The `user_id = $4` guard matches the two statements above and is here for the
// same reason, even though the §7.29 handler already establishes ownership: a
// consent record must not be writable against a car the acknowledging account
// does not hold, and defence that lives only in the caller is defence that the
// second caller will not have.
const queryAcknowledgeDriverAccess = `
UPDATE go_vehicle_driver_access
SET acknowledged_at = $2, acknowledgment_version = $3
WHERE vehicle_id = $1
  AND user_id = $4
  AND acknowledged_at IS NULL`

// ownerApprovalAuditMetadata is the whole P0 payload of the
// `vehicle.owner_approval_acknowledged` audit row: the copy version and
// nothing else (CG-DL-5).
//
// There is deliberately no VIN (P1), no owner (this platform cannot name them),
// and no rendered text (a published document with a stable id — storing a copy
// per row would duplicate something that must not vary per row). The PERSON is
// the row's userId and the CAR is its targetId, both already columns.
type ownerApprovalAuditMetadata struct {
	Version string `json:"version"`
}

// AcknowledgeOwnerApproval records that the driver of vehicleID acknowledged
// the owner's approval, under the copy version they were shown, and writes the
// matching audit row IN THE SAME TRANSACTION. Returns whether an
// UNACKNOWLEDGED driver-access row existed to stamp.
//
// ONE TRANSACTION, because the two writes are one fact from two directions: the
// standing row is the GATE ("this car may now be configured") and the audit row
// is the EVIDENCE ("this person agreed, then, to that"). A gate opened without
// evidence is the state nobody could later explain, and evidence written for a
// gate that never opened would be a record of a consent that had no effect.
//
// false + nil error is the ORDINARY answer for an owner's own car and the §7.29
// handler's 200 no-op path — never an error, because "this car needed no
// acknowledgment" is a fact about the car and not a fault of the request. NO
// AUDIT ROW IS WRITTEN in that case: nothing was acknowledged, and an audit
// trail that recorded non-events would be worse than useless in the one
// conversation it exists for.
//
// It does NOT push anything. The gate this opens is read by the caller, which
// performs the same best-effort push complete-setup performs, so the write and
// the Tesla call stay separable — and a push that fails cannot roll back a
// consent that was genuinely given.
func (r *VehicleRepo) AcknowledgeOwnerApproval(
	ctx context.Context, vehicleID, userID, version string, now time.Time,
) (bool, error) {
	if strings.TrimSpace(vehicleID) == "" || strings.TrimSpace(userID) == "" {
		return false, fmt.Errorf("store.AcknowledgeOwnerApproval: empty vehicle or user id")
	}
	meta, err := json.Marshal(ownerApprovalAuditMetadata{Version: version})
	if err != nil {
		return false, fmt.Errorf("store.AcknowledgeOwnerApproval: marshal audit metadata: %w", err)
	}

	start := time.Now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		r.metrics.IncQueryError("vehicle.acknowledge_owner_approval")
		return false, fmt.Errorf("VehicleRepo.AcknowledgeOwnerApproval(%s): begin: %w", vehicleID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, queryAcknowledgeDriverAccess, vehicleID, now, version, userID)
	if err != nil {
		r.metrics.IncQueryError("vehicle.acknowledge_owner_approval")
		return false, fmt.Errorf("VehicleRepo.AcknowledgeOwnerApproval(%s): %w", vehicleID, err)
	}
	if tag.RowsAffected() == 0 {
		// Nothing to record: an owner-access car (no row) or one already
		// acknowledged (the predicate excluded it). Commit the (empty)
		// transaction rather than rolling back so the two exits are
		// indistinguishable to anything watching.
		if err := tx.Commit(ctx); err != nil {
			r.metrics.IncQueryError("vehicle.acknowledge_owner_approval")
			return false, fmt.Errorf("VehicleRepo.AcknowledgeOwnerApproval(%s): commit: %w", vehicleID, err)
		}
		r.metrics.ObserveQueryDuration("vehicle.acknowledge_owner_approval", time.Since(start).Seconds())
		return false, nil
	}

	// Reuses the same-package queryAuditInsert column list — the single source
	// of truth shared with AuditRepo, which is what keeps CG-DL-8 column parity
	// automatic (the owner-teardown row is written the same way).
	if _, err := tx.Exec(ctx, queryAuditInsert,
		newProvisionID(), // id (cuid)
		userID,           // userId — the acknowledging driver
		now.UTC(),        // timestamp
		string(AuditActionOwnerApprovalAcknowledged), // action
		auditTargetTypeVehicle,                       // targetType
		vehicleID,                                    // targetId
		auditInitiatorUser,                           // initiator
		meta,                                         // metadata (P0: version only)
		now.UTC(),                                    // createdAt
	); err != nil {
		r.metrics.IncQueryError("vehicle.acknowledge_owner_approval")
		return false, fmt.Errorf("VehicleRepo.AcknowledgeOwnerApproval(%s): insert audit: %w", vehicleID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		r.metrics.IncQueryError("vehicle.acknowledge_owner_approval")
		return false, fmt.Errorf("VehicleRepo.AcknowledgeOwnerApproval(%s): commit: %w", vehicleID, err)
	}
	r.metrics.ObserveQueryDuration("vehicle.acknowledge_owner_approval", time.Since(start).Seconds())
	return true, nil
}

// GetDriverAccess reads one car's driver-access row.
//
// The §7.29 handler already holds a VehicleSnapshotRow (which carries the row
// through the snapshot's LEFT JOIN), so this exists for the OPS surfaces and
// for tests that assert the write half without going through a vehicle read.
// Absence is not an error — it is owner access, the zero value.
func (r *VehicleRepo) GetDriverAccess(ctx context.Context, vehicleID string) (VehicleDriverAccess, error) {
	var scan driverAccessScan
	err := r.pool.QueryRow(ctx,
		`SELECT created_at, acknowledged_at FROM go_vehicle_driver_access WHERE vehicle_id = $1`,
		vehicleID,
	).Scan(scan.dests()...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return VehicleDriverAccess{}, nil
		}
		return VehicleDriverAccess{}, fmt.Errorf("VehicleRepo.GetDriverAccess(%s): %w", vehicleID, err)
	}
	return scan.value(), nil
}
