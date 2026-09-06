package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The MYR-602 trips MUTATION path: patch, end, leave, and the revoked-share
// cascade.
//
// EVERY OWNER-SCOPED STATEMENT CARRIES `owner_user_id = $n` ITSELF. The
// handler's ownership check produces the good error message; the predicate is
// what actually prevents one person mutating another's trip, applied by the
// database to the same row it writes, so there is no check-then-write window.

// Update applies PATCH /api/trips/{id}. Owner-only; the caller has already been
// established as the owner, and the statements re-assert it anyway.
//
// REFUSES ON AN ENDED TRIP (ErrTripEnded). Every mutation this offers is about
// a live window: renaming a finished trip is pointless, adding somebody to one
// grants nothing, and EXTENDING `ends_at` past NOW() on a lapsed trip would
// RESURRECT live access that every participant was already told had ended.
// Continuing a road trip is a new trip, which says the true thing on
// everybody's phone.
//
// ONE TRANSACTION, and the vehicle is advisory-locked before the overlap probe
// for the same reason Create locks it: the probe is a read and the write that
// follows is what makes it wrong.
func (r *TripRepo) Update(ctx context.Context, tripID, ownerUserID string, in UpdateTripInput) (TripView, error) {
	const op = "trip.update"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Update(%s): begin: %w", tripID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := r.loadOwnedTripForPatch(ctx, tx, tripID, ownerUserID)
	if err != nil {
		if !errors.Is(err, ErrTripNotFound) && !errors.Is(err, ErrTripEnded) {
			r.metrics.IncQueryError(op)
		}
		return TripView{}, err
	}
	now := time.Now()

	if err := lockVehicleTrips(ctx, tx, current.VehicleID); err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Update(%s): %w", tripID, err)
	}

	nameEnc, endsAt, err := r.resolvePatch(current, in, now)
	if err != nil {
		return TripView{}, err
	}

	if !endsAt.Equal(current.EndsAt) {
		overlaps, probeErr := tripWindowOverlaps(ctx, tx, current.VehicleID, current.StartsAt, endsAt, tripID)
		if probeErr != nil {
			r.metrics.IncQueryError(op)
			return TripView{}, fmt.Errorf("TripRepo.Update(%s): %w", tripID, probeErr)
		}
		if overlaps {
			return TripView{}, ErrTripOverlap
		}
	}

	if _, err := tx.Exec(ctx, queryUpdateTripWindow, tripID, ownerUserID, nameEnc, endsAt); err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Update(%s): write: %w", tripID, err)
	}

	if err := applyRosterPatch(ctx, tx, tripID, current.VehicleID, in); err != nil {
		if !errors.Is(err, ErrTripParticipantNotShared) {
			r.metrics.IncQueryError(op)
		}
		return TripView{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		r.metrics.IncQueryError(op)
		return TripView{}, fmt.Errorf("TripRepo.Update(%s): commit: %w", tripID, err)
	}
	return r.GetForUser(ctx, tripID, ownerUserID)
}

// loadOwnedTripForPatch reads the row PATCH is about and applies the two gates
// that must hold before anything is written.
//
// THE OWNERSHIP CHECK IS BELT AND BRACES: the handler already resolved
// ownership through the vehicle, this row says who owns the TRIP, and the
// UPDATE statements carry `owner_user_id = $n` a third time. A car that changed
// hands is exactly the case where the three could disagree, and for a trip the
// trip's own owner column is the right authority.
//
// A NON-OWNER GETS ErrTripNotFound, not a denial — the 404-not-403 rule this
// whole surface follows, so PATCH is not an oracle for trip ids either.
func (r *TripRepo) loadOwnedTripForPatch(ctx context.Context, tx tripQuerier, tripID, ownerUserID string) (TripView, error) {
	current, err := r.scanTripRow(tx.QueryRow(ctx, queryTripByIDForUser, ownerUserID, tripID))
	if err != nil {
		if errors.Is(err, ErrTripNotFound) {
			return TripView{}, ErrTripNotFound
		}
		return TripView{}, fmt.Errorf("TripRepo.Update(%s): %w", tripID, err)
	}
	if current.Role != tripRoleOwner || current.OwnerUserID != ownerUserID {
		return TripView{}, ErrTripNotFound
	}
	if current.StatusAt(time.Now()) == TripStatusEnded {
		return TripView{}, ErrTripEnded
	}
	return current, nil
}

// applyRosterPatch adds and removes participants inside the patch transaction.
//
// ADDITIONS BEFORE REMOVALS is not arbitrary: an id in both lists ends up
// REMOVED, which is the safer resolution of a contradictory request. The
// contract does not define that case, so the server picks the answer that
// grants less.
//
// Split out of Update purely for the cognitive-complexity budget, but the seam
// is a real one: this is the whole of "who is on the trip", and Update is the
// whole of "what the trip is".
func applyRosterPatch(ctx context.Context, tx tripQuerier, tripID, vehicleID string, in UpdateTripInput) error {
	added, err := resolveShareParticipants(ctx, tx, vehicleID, in.AddParticipantIDs)
	if err != nil {
		if errors.Is(err, ErrTripParticipantNotShared) {
			return err
		}
		return fmt.Errorf("TripRepo.Update(%s): %w", tripID, err)
	}
	if err := addTripParticipants(ctx, tx, tripID, added); err != nil {
		return fmt.Errorf("TripRepo.Update(%s): %w", tripID, err)
	}
	if err := removeParticipantsByShare(ctx, tx, tripID, in.RemoveParticipantIDs); err != nil {
		return fmt.Errorf("TripRepo.Update(%s): %w", tripID, err)
	}
	return nil
}

// tripRoleOwner is the value tripRoleExpr emits for an owner. Named rather than
// spelled inline so the SQL and the Go comparison move together.
const tripRoleOwner = "owner"

// resolvePatch folds the optional fields onto the current row and validates the
// result.
//
// THE WHOLE WINDOW IS RE-VALIDATED, not just the field that moved. A patch that
// only checked its own delta could walk a trip into a state no create would
// have accepted, one legal-looking step at a time.
func (r *TripRepo) resolvePatch(current TripView, in UpdateTripInput, now time.Time) (nameEnc string, endsAt time.Time, err error) {
	name := current.Name
	if in.Name != nil {
		validated, nameErr := ValidateTripName(*in.Name)
		if nameErr != nil {
			return "", time.Time{}, nameErr
		}
		name = validated
	}
	sealed, sealErr := labelToEncString(name, r.encryptor)
	if sealErr != nil {
		// The plaintext stays out of the error: it is P1 user content.
		return "", time.Time{}, fmt.Errorf("seal trip name: %w", sealErr)
	}

	endsAt = current.EndsAt
	if in.EndsAt != nil {
		endsAt = *in.EndsAt
		// SHORTENING MUST NOT REACH INTO THE PAST. `endsAt` in the past would
		// end the trip retroactively — a different operation with a different
		// name (`POST /end`), which stamps `ended_at` and leaves the owner's
		// stated window intact so an accidental early end stays explainable.
		// Rejecting here is what keeps the two from being the same button.
		if endsAt.Before(now) {
			return "", time.Time{}, fmt.Errorf("%w: endsAt is in the past — end the trip instead", ErrTripWindowInvalid)
		}
	}
	if err := validateTripWindow(current.StartsAt, endsAt); err != nil {
		return "", time.Time{}, err
	}
	return sealed, endsAt, nil
}

// removeParticipantsByShare marks the named memberships left. Keyed on the
// SHARE id, which is what the wire calls `participantId`.
//
// Silently tolerant of an id that names nobody on this trip: removing a person
// who is not there is the state the caller asked for, and erroring would make
// PATCH fail on a double-tap.
func removeParticipantsByShare(ctx context.Context, q tripQuerier, tripID string, shareIDs []string) error {
	for _, shareID := range dedupeStrings(shareIDs) {
		if _, err := q.Exec(ctx, `
UPDATE go_trip_participants
SET left_at = NOW()
WHERE trip_id = $1 AND share_id = $2 AND left_at IS NULL`, tripID, shareID); err != nil {
			return fmt.Errorf("remove participant: %w", err)
		}
	}
	return nil
}

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
