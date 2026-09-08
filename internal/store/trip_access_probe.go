package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// THE CHEAP ROLE PROBE, shared by the two MYR-618 capability routes.
//
// It is its own file rather than a helper at the top of the add path, because
// its two callers pull in opposite directions — trip_repo_participants.go is a
// transaction that WIDENS a roster, trip_repo_addable_people.go is a read that
// can grant nothing — and the property that matters is that both reach the
// 404-not-403 rule through exactly ONE definition of "on this trip". Splitting
// it here is what makes that visible; leaving it in either caller would make it
// look like that caller's private helper.
//
// ⚠ ITS STATEMENT IS THE ONE THAT CARRIES tripMemberRoleExpr rather than
// tripRoleExpr — the live-grant re-join (invariant 3 in trip_queries.go). The
// difference is the whole security argument and is written out at both
// expressions; the short version is that these two routes ask "may this person
// ACT as a member", not "is this person on the roster", and a suspended or
// revoked grant-holder must fail the first while still passing the second.

// tripAccessRow is the cheap role probe's result: the caller's relationship to
// a trip plus the three columns that decide whether the trip is still live.
//
// Deliberately NOT a TripView. The full read decrypts a name, resolves a trim,
// counts drives and reads a leg — five round trips to answer a question that is
// "may this person act on this trip, and is it over?". A gate that costs that
// much is a gate somebody eventually caches.
type tripAccessRow struct {
	VehicleID   string
	OwnerUserID string
	StartsAt    time.Time
	EndsAt      time.Time
	EndedAt     *time.Time
	Role        string
}

// ended reports whether the window has closed, by the SAME rule Trip.StatusAt
// applies: a stamped `ended_at` is terminal on its own, and otherwise the
// scheduled end decides.
func (r tripAccessRow) ended(now time.Time) bool {
	if r.EndedAt != nil {
		return true
	}
	return !now.Before(r.EndsAt)
}

// tripAccessFor resolves the caller's role on one trip, or ErrTripNotFound.
//
// ONE ANSWER FOR "NO SUCH TRIP" AND "NOT YOUR TRIP", the 404-not-403 rule this
// surface is built on, applied through the same `tripRoleExpr` every other read
// uses rather than through a second predicate that could drift from it.
func tripAccessFor(ctx context.Context, q tripQuerier, tripID, userID string) (tripAccessRow, error) {
	var (
		row  tripAccessRow
		role *string
	)
	err := q.QueryRow(ctx, queryTripRoleForUser, userID, tripID).Scan(
		&row.VehicleID, &row.OwnerUserID, &row.StartsAt, &row.EndsAt, &row.EndedAt, &role,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return tripAccessRow{}, ErrTripNotFound
	case err != nil:
		return tripAccessRow{}, fmt.Errorf("trip access probe: %w", err)
	}
	if role == nil || *role == "" {
		return tripAccessRow{}, ErrTripNotFound
	}
	row.Role = *role
	return row, nil
}
