package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// THE THIRD WAY A TRIP STOPS BEING YOURS (MYR-607, rest-api.md §7.30.10): the
// owner deletes it.
//
// Its two neighbours in trip_repo_end.go both leave a ROW behind — an ended
// trip is still a trip, a left participant is still a tombstone — and both are
// idempotent because they move an access boundary. This one leaves NOTHING
// behind, and its idempotency is of a different kind: a second call finds no
// row and answers ErrTripNotFound, which the handler renders as the 404 it
// gives a stranger. **From the client's side that is indistinguishable from
// success, and deliberately so** — a delete that answered 404 on the retry of
// its own timed-out request would be a bug the app could not tell from a bug in
// the server.
//
// ⚠ THE DRIVES ARE UNTOUCHED, and that is the whole reason this is a delete at
// all rather than a refusal. A trip never OWNED a drive — the window merely
// SELECTED it, by time, from a car's own history — so deleting the window
// deletes the selection and nothing else. The same argument
// docs/architecture/trips.md §9 makes for account deletion, made once more here
// because it is the sentence the confirm dialog puts in front of the owner.

// Delete removes one trip and its three children, owner-scoped.
//
// FIVE STATEMENTS FOR FOUR TABLES, in one transaction, in child-first order,
// even though migration 0047's foreign keys would cascade all four from the
// parent alone. The explicitness is deliberate on two counts:
//
//   - go_live_activities is reached only THROUGH go_trip_legs (its anchor FK),
//     so the cascade that covers it is two links long. A statement that names
//     it is a statement a reader can find when they ask "does deleting a trip
//     take the lock-screen registrations with it".
//   - The account-deletion sequence learned the same lesson from the other
//     side (§9): what a cascade covers is invisible at the call site, and the
//     row it silently missed would be a live capability addressed at a phone.
//
// The parent DELETE is still owner-scoped and still the one that decides: the
// FOR UPDATE probe above it establishes ownership and locks the row for the
// length of the transaction, so two concurrent deletes serialise and the loser
// finds nothing.
//
// AUDIT FIRST, INSIDE THE SAME TRANSACTION (CG-DL-3). A `trip.deleted` row that
// committed without the delete would be a lie; a delete that committed without
// the row would be the state nobody could later explain. Both go together or
// neither does.
func (r *TripRepo) Delete(ctx context.Context, tripID, ownerUserID string) error {
	const op = "trip.delete"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		r.metrics.IncQueryError(op)
		return fmt.Errorf("TripRepo.Delete(%s): begin: %w", tripID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	vehicleID, err := lockTripForOwner(ctx, tx, tripID, ownerUserID)
	if err != nil {
		if errors.Is(err, ErrTripNotFound) {
			return ErrTripNotFound
		}
		r.metrics.IncQueryError(op)
		return fmt.Errorf("TripRepo.Delete(%s): %w", tripID, err)
	}

	if err := insertTripDeletedAudit(ctx, tx, ownerUserID, tripID, vehicleID); err != nil {
		r.metrics.IncQueryError(op)
		return fmt.Errorf("TripRepo.Delete(%s): %w", tripID, err)
	}

	for _, stmt := range tripDeleteChildStatements {
		if _, err := tx.Exec(ctx, stmt, tripID); err != nil {
			r.metrics.IncQueryError(op)
			return fmt.Errorf("TripRepo.Delete(%s): delete children: %w", tripID, err)
		}
	}

	tag, err := tx.Exec(ctx, queryDeleteTrip, tripID, ownerUserID)
	if err != nil {
		r.metrics.IncQueryError(op)
		return fmt.Errorf("TripRepo.Delete(%s): delete trip: %w", tripID, err)
	}
	if tag.RowsAffected() == 0 {
		// Unreachable through the probe above, which held the row locked. Kept
		// because the probe and the delete are two statements and a future
		// edit could separate them: a zero-row parent delete must not commit a
		// transaction that has already removed the children.
		return ErrTripNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		r.metrics.IncQueryError(op)
		return fmt.Errorf("TripRepo.Delete(%s): commit: %w", tripID, err)
	}
	return nil
}

// lockTripForOwner is the ownership gate and the serialiser in one statement.
//
// ErrTripNotFound covers BOTH an unknown id and a trip somebody else owns —
// including one the caller is merely a PARTICIPANT of — because a trip the
// caller may not act on must be indistinguishable from a trip that does not
// exist. That is §7.30's 404-not-403 rule, enforced in the statement rather
// than in the handler.
//
// It returns the vehicle id, which the audit row's metadata carries: the row is
// filed against the trip, and the car is the only other thing about a deleted
// window that is still nameable afterwards.
func lockTripForOwner(ctx context.Context, tx pgx.Tx, tripID, ownerUserID string) (string, error) {
	var vehicleID string
	err := tx.QueryRow(ctx, queryLockTripForOwner, tripID, ownerUserID).Scan(&vehicleID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrTripNotFound
	case err != nil:
		return "", fmt.Errorf("lock trip: %w", err)
	}
	return vehicleID, nil
}

// tripDeletedAuditMetadata is the `trip.deleted` row's metadata: ONE OPAQUE
// CUID IN THE `metadata` COLUMN, which is TWO across the whole row once
// `targetId` (the trip) is counted — the figure rest-api.md §7.30.10 and
// data-lifecycle.md §4.2 quote. P0 throughout (CG-DL-5). Its MYR-618 sibling
// carries one more, the share id, and its comment counts the same two ways.
//
// Deliberately NOT the trip name — P1 user content sealed at rest
// (data-classification.md §1.25), and an audit row is a place a value reaches
// permanent storage without anybody deciding it should. Deliberately not the
// participant list either: who was on somebody's road trip is a fact about
// those people, and they are not the subject of this row.
type tripDeletedAuditMetadata struct {
	VehicleID string `json:"vehicleId"`
}

// insertTripDeletedAudit writes the user-initiated `trip.deleted` AuditLog row
// inside the deletion transaction and BEFORE the deletes (CG-DL-3), through the
// shared writer in trip_audit.go.
func insertTripDeletedAudit(ctx context.Context, tx pgx.Tx, ownerUserID, tripID, vehicleID string) error {
	return insertTripAudit(ctx, tx, AuditActionTripDeleted, ownerUserID, tripID,
		tripDeletedAuditMetadata{VehicleID: vehicleID})
}
