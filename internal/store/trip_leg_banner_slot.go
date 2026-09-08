package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ONE BANNER PER (TRIP, DESTINATION) PER WINDOW (MYR-620).
//
// 2026-09-08, a client screenshot: TEN "Tesla is on the move — Heading to
// Element by Marriott Sedona." banners on one lock screen in 59 minutes, five
// of them inside a single minute. Every one was a correctly-claimed,
// once-per-leg `trip_leg_started` push. The LEG is what flapped (MYR-612), and
// `started_notified_at` cannot see that: it is a claim on a ROW, and each
// reopen was a new row.
//
// MYR-612's debounce and resume make the flap far rarer. This claim makes the
// banner bounded whatever the detector does, which is the property the person
// holding the phone cares about.
//
// Split into its own file, beside trip_leg_repo.go's four per-leg claims,
// because it answers a different question of a different subject: those
// arbitrate DELIVERY of one leg's four independent sends, this one asks whether
// this SENTENCE has been said about this TRIP recently.

// queryClaimLegBannerSlot takes the (trip, event, destination) slot when it is
// free or stale, and reports nothing when a banner for the same sentence went
// out inside the window.
//
// UPSERT-AS-CLAIM. The first banner for a destination INSERTs; a later one
// takes the slot only if the stored stamp is older than `$5` (now minus the
// window). Two servers racing on one flap resolve at the row lock, so exactly
// one of them sends — which the alternative shape, SELECT-then-UPDATE, cannot
// promise.
//
// THE STAMP IS ADVANCED ONLY BY A WINNER. A suppressed banner leaves
// `last_sent_at` alone, so the window measures time since the last banner
// actually SENT rather than since the last one attempted — twenty attempts in a
// minute do not push the next legitimate banner half an hour further out.
const queryClaimLegBannerSlot = `
INSERT INTO go_trip_leg_banners (trip_id, event, destination_key, last_sent_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (trip_id, event, destination_key) DO UPDATE
SET last_sent_at = EXCLUDED.last_sent_at
WHERE go_trip_leg_banners.last_sent_at < $5
RETURNING trip_id`

// ClaimLegBannerSlot reports whether this banner may be sent.
//
// `destinationKey` is a DIGEST of the normalised destination name, never the
// name: a destination is P1 and this table holds none. internal/trips computes
// it, because that is where the plaintext already is.
//
// AN EMPTY KEY IS ALWAYS ALLOWED, deliberately. A leg with no destination name
// is a leg whose banner says nothing that could be repeated, and suppressing on
// a key that every such leg shares would collapse genuinely different journeys
// into one slot.
func (r *TripLegRepo) ClaimLegBannerSlot(
	ctx context.Context, tripID, event, destinationKey string, now time.Time, window time.Duration,
) (bool, error) {
	if tripID == "" || event == "" || destinationKey == "" {
		return true, nil
	}
	var id string
	err := r.pool.QueryRow(ctx, queryClaimLegBannerSlot,
		tripID, event, destinationKey, now, now.Add(-window)).Scan(&id)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("store.ClaimLegBannerSlot(trip=%s, event=%s): %w", tripID, event, err)
	}
}
