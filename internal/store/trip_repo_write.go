package store

import (
	"context"
	"errors"
	"fmt"
	"time"
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
