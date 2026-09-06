package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// WHO MAY REGISTER A CARD ON THIS LEG — the §7.21.7 route's whole gate, in one
// statement.
//
// Split from live_activity_trip_anchor.go so both stay inside the 300-line cap,
// and along the seam that matters: everything next door is about the ROW (how a
// leg-anchored Activity is written, listed, stamped and tombstoned), while this
// is the only ACCESS question in the file. The route carries no trip id, so
// this statement IS the authorization — and its two answers have to stay
// distinguishable, which is why it returns a flag rather than folding both into
// an error.

// queryTripLegAccess answers "is this person on the trip this leg belongs to,
// and is the leg still open?" — the §7.21.7 registration route's whole gate.
//
// THE ROUTE CARRIES NO TRIP ID (`/api/trip-legs/{legId}/activity-token`), which
// is the right shape — the device is holding one leg's card and a leg belongs to
// exactly one trip, so making the client restate the trip would be asking it to
// prove something the server already knows. The consequence is that the
// authorization has to be resolved FROM the leg, here, in one statement.
//
// TWO ANSWERS, AND THEY MUST STAY DISTINGUISHABLE at the handler even though
// they are one row: no row at all is "unknown leg, or not your trip" (404, one
// answer for both so the endpoint cannot be used to probe leg ids), and a row
// with `open = false` is "your leg, and it has ended" (409, end the card
// locally). Collapsing them would either turn a stranger's probe into a 409
// that confirms the leg exists, or turn a legitimate closed-leg refusal into a
// 404 the client would read as "this card was never real".
//
// THE MEMBERSHIP ARM IS THE SAME PREDICATE PAIR EVERY TRIPS SURFACE CARRIES —
// `left_at IS NULL` and `status = 'accepted' AND suspended_at IS NULL` — so a
// departed or suspended person is refused here exactly as they are refused a
// push, a card and a coordinate. The OWNER arm needs no share: they hold no
// grant on their own car, and they are on the leg card by explicit product
// decision.
//
// NO WINDOW PREDICATE, deliberately. A leg exists only inside a window and is
// closed when the window closes (SettleTrip ends every open leg), so the leg's
// own `ended_at` already carries the window's edge — and re-asking the window
// here would make a card registered a second after a leg closed fail with a
// DIFFERENT error than one registered a second later still.
const queryTripLegAccess = `
SELECT l.trip_id, (l.ended_at IS NULL)
FROM go_trip_legs l
JOIN go_trips t ON t.id = l.trip_id
WHERE l.id = $1
  AND (
        t.owner_user_id = $2
     OR EXISTS (
            SELECT 1
            FROM go_trip_participants p
            JOIN go_vehicle_shares s
              ON s.vehicle_id = t.vehicle_id
             AND s.accepted_by_user_id = p.user_id
             AND s.status = 'accepted'
             AND s.suspended_at IS NULL
            WHERE p.trip_id = t.id AND p.user_id = $2 AND p.left_at IS NULL
        )
  )`

// TripLegAccess resolves the caller's standing on one leg: which trip it belongs
// to, and whether it is still open.
//
// Returns ErrTripNotFound when the leg does not exist OR the caller is not on
// its trip — ONE sentinel for both, deliberately, so the §7.21.7 route answers
// 404 identically in either case and cannot be used to discover leg ids.
//
// The `open` flag is returned rather than folded into the error because the two
// refusals are different answers to the client: a stranger gets 404 and stops,
// while a member whose leg has ended gets 409 and ends the card locally.
func (r *LiveActivityRepo) TripLegAccess(ctx context.Context, legID, userID string) (tripID string, open bool, err error) {
	if strings.TrimSpace(legID) == "" {
		return "", false, fmt.Errorf("store.TripLegAccess: empty leg id")
	}
	if strings.TrimSpace(userID) == "" {
		return "", false, fmt.Errorf("store.TripLegAccess(leg=%s): empty user id", legID)
	}
	err = r.pool.QueryRow(ctx, queryTripLegAccess, legID, userID).Scan(&tripID, &open)
	switch {
	case err == nil:
		return tripID, open, nil
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, fmt.Errorf("store.TripLegAccess(leg=%s, user=%s): %w",
			legID, userID, ErrTripNotFound)
	default:
		return "", false, fmt.Errorf("store.TripLegAccess(leg=%s, user=%s): %w", legID, userID, err)
	}
}
