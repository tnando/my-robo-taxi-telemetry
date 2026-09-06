package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// THE TWO WAYS A TRIP STOPS BEING YOURS: the owner ends it, or a participant
// leaves it.
//
// Split from trip_repo_write.go so both stay inside the 300-line cap, and along
// a seam that is not arbitrary: PATCH there is an edit — it changes what a trip
// SAYS — while both of these change WHO CAN SEE A CAR, and both are idempotent
// for the same reason. Re-stamping either would move an access boundary on
// every retry.

// End stamps the owner's early end and returns the trip.
//
// IDEMPOTENT. The statement's `ended_at IS NULL` guard means a second call
// writes nothing and the re-read returns the already-ended trip, so the
// endpoint answers 200 either way. Re-stamping would move the end FORWARD on
// every retry, which for an access boundary means a double-tap silently
// extending somebody's live location by however long the two taps were apart.
func (r *TripRepo) End(ctx context.Context, tripID, ownerUserID string) (TripView, error) {
	const op = "trip.end"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	// Read first, so a trip the caller does not own is 404 rather than a
	// zero-row UPDATE indistinguishable from an idempotent repeat.
	current, err := r.scanTripRow(r.pool.QueryRow(ctx, queryTripByIDForUser, ownerUserID, tripID))
	if err != nil {
		if errors.Is(err, ErrTripNotFound) {
			return TripView{}, ErrTripNotFound
		}
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.End(%s): %w", tripID, err)
	}
	if current.Role != tripRoleOwner || current.OwnerUserID != ownerUserID {
		return TripView{}, ErrTripNotFound
	}

	if _, err := r.pool.Exec(ctx, queryEndTrip, tripID, ownerUserID); err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.End(%s): %w", tripID, err)
	}
	return r.GetForUser(ctx, tripID, ownerUserID)
}

// Leave marks the caller's own membership left (DELETE …/participants/me).
//
// IDEMPOTENT AND SILENT. It reports nothing about whether the trip exists,
// whether the caller was ever on it, or whether they had already left, and the
// handler answers 204 in every case. That is the same non-oracle posture the
// read path takes: a 404 here would tell any authenticated caller which trip
// ids are real.
//
// Returns whether a row actually moved, so the caller can decide whether a
// push or a cache bust is warranted without a second query.
func (r *TripRepo) Leave(ctx context.Context, tripID, userID string) (bool, error) {
	const op = "trip.leave"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	tag, err := r.pool.Exec(ctx, queryLeaveTrip, tripID, userID)
	if err != nil {
		r.metrics.IncQueryError(op)
		return false, fmt.Errorf("TripRepo.Leave(%s): %w", tripID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// RemoveParticipantsForShare is the REVOKED-SHARE CASCADE: when a grant on a
// car is revoked or handed back, the person drops out of that car's live trips.
//
// ⚠ THIS IS NOT WHAT ENFORCES THE ACCESS RULE, and mistaking it for that would
// be the dangerous reading. Trip access can never outlive the share because
// EVERY access query re-joins the live grant — internal/auth's fourth UNION
// leg, queryActiveTripParticipation, queryTripWindowsForUserVehicle and the
// catalog leg all carry `status = 'accepted' AND suspended_at IS NULL`. A
// revoked grant therefore stops granting the instant it is revoked, cascade or
// no cascade, and if this function never ran the security property would still
// hold.
//
// What it fixes is the ROSTER. Without it the owner's trip card keeps listing
// somebody who can no longer see anything, the participant count lies, and the
// person appears in the "who is on this trip" list of a car they have been
// removed from. It is a display-consistency repair, and it is written down as
// one so nobody later "optimises" the access predicates away on the strength of
// it.
//
// Scoped to trips that have NOT ended: rewriting a finished trip's roster would
// rewrite history for no benefit — the window is closed, the access is already
// gone, and the roster is the only record of who was on it.
func (r *TripRepo) RemoveParticipantsForShare(ctx context.Context, vehicleID, userID string) (int, error) {
	const op = "trip.cascade_share_revoke"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	if vehicleID == "" || userID == "" {
		// A revoke on an invite nobody redeemed has no user to cascade for.
		// Not an error: there is simply no membership that could exist.
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, queryLeaveTripByShare, vehicleID, userID)
	if err != nil {
		r.metrics.IncQueryError(op)
		return 0, fmt.Errorf("TripRepo.RemoveParticipantsForShare(vehicle=%s): %w", vehicleID, err)
	}
	return int(tag.RowsAffected()), nil
}

// compile-time assertion that pgx.Tx satisfies tripQuerier, so the shared
// helpers really do run in either context.
var _ tripQuerier = pgx.Tx(nil)
