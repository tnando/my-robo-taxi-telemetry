package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// THE ONE WRITER OF EVERY TRIP-SCOPED AuditLog ROW.
//
// `trip.deleted` (MYR-607, rest-api.md §7.30.10) and `trip.participant_added`
// (MYR-618, §7.30.4) are the two members today, and until the review round each
// had its own copy of the same nine-argument Exec — same column list, same
// `targetType`, same `initiator`, same UTC clock read once and used for both
// timestamps, differing in the action string and the metadata struct.
//
// ⚠ IT IS EXTRACTED FOR CORRECTNESS RATHER THAN FOR BREVITY. Nine positional
// arguments against a nine-column INSERT is a shape where a transposition — the
// actor and the target id are both cuids, `timestamp` and `createdAt` are both
// timestamps — compiles, runs, and produces an audit trail that is wrong in a
// way nobody notices until the one conversation the trail exists for. One
// writer means one place to get that order right, and it is the same discipline
// that put every trip STATEMENT in trip_queries.go.
//
// It reuses the same-package `queryAuditInsert` column list — the single source
// of truth `AuditRepo` itself writes through — which is what keeps CG-DL-8
// column parity automatic rather than remembered.
//
// ── WHAT IT DOES NOT DO ─────────────────────────────────────────────────────
//
// It does not choose the transaction, does not decide WHETHER a row is
// warranted, and does not know the P0/P1 rules. Its callers do: `Delete` writes
// exactly one row before its deletes (CG-DL-3), and `addAndAuditParticipants`
// writes one per person ACTUALLY added and none for a no-op re-send. A helper
// that also decided when to write would make "a no-op writes nothing" a
// property of this file rather than of the roster diff, which is where it is
// visible.

// insertTripAudit writes one trip-scoped AuditLog row inside the caller's
// transaction.
//
// `actorUserID` is the `userId` column and it is THE PERSON WHO ACTED, which
// since MYR-618 is not necessarily the trip's owner — that variability is the
// whole reason `trip.participant_added` exists as a row.
//
// `meta` is marshalled to the `metadata` column and MUST be P0-only (CG-DL-5).
// Both current shapes hold opaque cuids and nothing else; a trip NAME is P1
// user content sealed at rest (data-classification.md §1.25) and must never
// reach this argument, on this path or on an error path.
func insertTripAudit(
	ctx context.Context, tx tripQuerier, action AuditAction, actorUserID, tripID string, meta any,
) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}
	// ONE CLOCK READ FOR BOTH COLUMNS. `timestamp` is when the act happened and
	// `createdAt` is when the row was written; they are the same instant here
	// by construction, and two reads could report two.
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, queryAuditInsert,
		newProvisionID(),    // id (cuid)
		actorUserID,         // userId — the person who acted
		now,                 // timestamp
		string(action),      // action
		auditTargetTypeTrip, // targetType
		tripID,              // targetId
		auditInitiatorUser,  // initiator
		raw,                 // metadata (P0 only, CG-DL-5)
		now,                 // createdAt
	); err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}
