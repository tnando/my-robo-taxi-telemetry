package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The ownership predicate the sharing writers share. One function, two callers
// — §7.5.1 create over its whole `vehicleIds` set and §7.5.8 extend over the
// single path vehicle — split out of vehicle_share_repo.go under the 300-line
// rule when MYR-609 made it shared rather than create-private.

// verifyOwnsAll fails unless every requested vehicle belongs to the owner. It
// compares COUNTS of distinct ids rather than checking membership one by one:
// a duplicate id in the request must not let a foreign id slip through the
// tally.
//
// SHARED BY CREATE AND EXTEND (MYR-609). Extend checks exactly one vehicle,
// and a one-element slice through this function is the same check rather than
// a second spelling of it — a private `verifyOwnsVehicle` next to it would
// have been a copy that could drift on the day the ownership relation moves.
// `caller` names the operation for the wrapped transport error, which is the
// only thing that differed between the two.
//
// It is a SECOND check on the extend path, not the only one: the handler
// already refused a non-owner with a 403 naming the vehicle. Both exist for the
// reason the top of vehicle_share_queries.go gives — the handler's check is the
// good error message, the one inside the transaction is what holds under
// concurrency and under a future caller who forgets. READ-ONLY against the
// sibling-owned vehicle relation (CG-DL-9 permits reads).
func verifyOwnsAll(ctx context.Context, tx pgx.Tx, caller, ownerID string, vehicleIDs []string) error {
	want := make(map[string]struct{}, len(vehicleIDs))
	for _, id := range vehicleIDs {
		want[id] = struct{}{}
	}

	rows, err := tx.Query(ctx, queryShareOwnedVehicleIDs, vehicleIDs, ownerID)
	if err != nil {
		return fmt.Errorf("%s(owner=%s): ownership check: %w", caller, ownerID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("%s(owner=%s): ownership scan: %w", caller, ownerID, err)
		}
		delete(want, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s(owner=%s): ownership iterate: %w", caller, ownerID, err)
	}
	if len(want) > 0 {
		return ErrShareVehicleNotOwned
	}
	return nil
}
