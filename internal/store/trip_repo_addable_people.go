package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// §7.30.11's read, split out of trip_repo_participants.go for the 300-line cap
// — but the seam is a real one rather than an arithmetic convenience. That file
// is the WRITE path: one transaction that widens a roster and audits it. This
// is the READ that feeds a picker, it takes no transaction, it can grant
// nothing, and its whole argument is about what it must NOT return.

// AddablePeople answers §7.30.11: the vehicle's live grant-holders who are not
// already on this trip, for a caller who owns the trip or is on it.
//
// TWO STATEMENTS, AND THE FIRST ONE IS THE GATE. The list statement returns no
// rows for a stranger anyway, but an empty list is also the honest answer for a
// trip where everybody is already aboard — so without the role probe the two
// would be indistinguishable, and the endpoint would answer 200 to somebody who
// must receive the same 404 every other per-trip route gives them.
func (r *TripRepo) AddablePeople(ctx context.Context, tripID, userID string) ([]TripAddablePersonView, error) {
	const op = "trip.addable_people"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	if _, err := tripAccessFor(ctx, r.pool, tripID, userID); err != nil {
		if !errors.Is(err, ErrTripNotFound) {
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("TripRepo.AddablePeople(%s): %w", tripID, err)
		}
		return nil, ErrTripNotFound
	}

	rows, err := r.pool.Query(ctx, queryTripAddablePeople, tripID)
	if err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.AddablePeople(%s): %w", tripID, err)
	}
	defer rows.Close()

	out := make([]TripAddablePersonView, 0, 8)
	for rows.Next() {
		var (
			person         TripAddablePersonView
			label          string
			acceptedByName *string
		)
		if err := rows.Scan(&person.ShareID, &label, &acceptedByName); err != nil {
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("TripRepo.AddablePeople(%s): scan: %w", tripID, err)
		}
		// THE SAME NAME RULE THE ROSTER USES (MYR-581/583), applied by the same
		// helper: the accepting account's confirmed first name wins, the
		// owner's own label for the grant is the fallback. A person must not be
		// called one thing in the picker and another thing on the roster one
		// tap later.
		person.DisplayName = label
		if first := ownerFirstNameToken(acceptedByName); first != nil {
			person.DisplayName = *first
		}
		out = append(out, person)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("TripRepo.AddablePeople(%s): rows: %w", tripID, err)
	}

	// Ordered for the reader, here rather than in SQL — see the statement's own
	// comment. The share id is the tie-break, so the order is TOTAL and a picker
	// showing two people the owner labelled the same does not reshuffle them
	// between reads.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DisplayName != out[j].DisplayName {
			return out[i].DisplayName < out[j].DisplayName
		}
		return out[i].ShareID < out[j].ShareID
	})
	return out, nil
}
