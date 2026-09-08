package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The AUDIT half of MYR-609 §7.5.8 extend — the action constant, its metadata
// shape, and the in-transaction writer. Split from vehicle_share_extend.go
// under the 300-line rule; the seam is a real one, because this is the only
// part of the feature that exists to be READ LATER rather than to decide
// anything now.

// AuditActionShareExtended records that an owner extended one of their
// ACCEPTED share grants onto another car they own (MYR-609).
//
// IT IS THE ONLY RECORD THAT THE ACCESS WAS NOT REDEEMED. Every other accepted
// row in go_vehicle_shares got there because somebody presented a code; this
// one got there because an owner pressed a button, and the row itself cannot
// say so — an extended grant is byte-for-byte an ordinary accepted grant, which
// is the point (every gate must treat it as one). So the audit row is where the
// distinction lives, and it is what answers "how did this person get access to
// this car when no invite for it was ever redeemed?".
//
// Metadata is TWO OPAQUE CUIDS AND NOTHING ELSE (CG-DL-5, P0-only): the new
// grant's id and the source grant's id. Deliberately no `label` and no `code` —
// both P1 (data-classification.md §1.15) — and deliberately not the GRANTEE's
// id either: the row is filed against the vehicle under the owner who acted,
// the two share ids resolve to the person for anybody with the database, and an
// audit row is a place a value reaches permanent storage without anybody
// deciding it should.
//
// DOTTED, like `trip.deleted` and `vehicle.owner_approval_acknowledged`: a
// share-scoped sub-action rather than a platform lifecycle verb.
const AuditActionShareExtended AuditAction = "share.extended"

// shareExtendedAuditMetadata is the `share.extended` row's metadata: two opaque
// cuids and nothing else (CG-DL-5, P0-only). See AuditActionShareExtended for
// what is deliberately absent and why.
type shareExtendedAuditMetadata struct {
	ShareID       string `json:"shareId"`
	SourceShareID string `json:"sourceShareId"`
}

// insertShareExtendedAudit writes the user-initiated `share.extended` AuditLog
// row inside the extend transaction, reusing the same-package queryAuditInsert
// column list (single source of truth shared with AuditRepo — keeps CG-DL-8
// column parity automatic).
//
// `targetType` is the VEHICLE and `targetId` the car gaining the grant, not the
// share row: the question this row is kept to answer is "who could see this car
// in June", which is asked about a car.
func insertShareExtendedAudit(ctx context.Context, tx pgx.Tx, in ExtendShareInput, shareID string) error {
	meta, err := json.Marshal(shareExtendedAuditMetadata{
		ShareID:       shareID,
		SourceShareID: in.SourceShareID,
	})
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, queryAuditInsert,
		newProvisionID(),                 // id (cuid)
		in.OwnerUserID,                   // userId (the owner who extended it)
		now,                              // timestamp
		string(AuditActionShareExtended), // action
		auditTargetTypeVehicle,           // targetType
		in.TargetVehicleID,               // targetId
		auditInitiatorUser,               // initiator
		meta,                             // metadata (two opaque cuids)
		now,                              // createdAt
	); err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}
