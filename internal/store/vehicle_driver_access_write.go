// The ACKNOWLEDGMENT half of MYR-599: stamping the consent that opens the
// config-push gate, and the audit row that is its evidence.
//
// FILING and CLEARING the gate row itself do NOT live here. They belong to the
// provisioning transaction — owner_vehicle_driver_gate.go — because a car that
// exists without its gate is indistinguishable from an owner's, so the two
// writes must be indivisible. This file once carried VIN-keyed RecordDriverAccess
// / ClearDriverAccess / GetDriverAccess siblings from the pre-transaction design;
// they were deleted once nothing but their own tests called them, since a second
// spelling of a consent write is a second thing a later change can pick by
// mistake.
//
// Split from vehicle_driver_access.go for the CLAUDE.md 300-line file cap; the
// read projection and these statements are one component.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// queryDeleteDriverAccessByVehicle is the VEHICLE-ID-KEYED delete used INSIDE
// the provisioning transaction (MYR-599) and by the owner-wins transfer.
//
// KEYED BY ID, and there is no VIN-keyed sibling any more. There used to be one
// — the pre-transaction design resolved the cuid through a SELECT on "Vehicle"
// because its caller held only a VIN — and it is gone with the rest of that
// design. The provisioning transaction has this row's id in hand from the
// upsert's RETURNING, so it needs no resolution, and MUST NOT do one: a SELECT
// inside the same transaction would be reading a row this transaction has not
// committed. Keying by id is also what makes the write ATOMIC with the vehicle
// it gates.
//
// The matching INSERT lives in owner_vehicle_driver_gate.go as
// queryGateDriverAccessByVehicle, because it is not a plain upsert: it carries
// the bound that stops a non-OWNER signal converting an established owner's car
// into a gated one.
//
// No `"userId"` guard is needed here: the id came from an upsert whose own WHERE
// clause already refused to touch a car belonging to anybody else, so there is
// no window in which the owner could have changed.
const queryDeleteDriverAccessByVehicle = `
DELETE FROM go_vehicle_driver_access WHERE vehicle_id = $1`

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
// The `user_id = $4` guard is here even though the §7.29 handler already
// establishes ownership, and for the plainest of reasons: a
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
