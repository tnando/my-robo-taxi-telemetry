package trips

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/push"
)

// A LEG'S LIVE ACTIVITY: raising it, refreshing the person who arrived late,
// and taking it off the lock screen.
//
// Split from legs.go under the 300-line cap, along the seam MYR-620 made
// structural rather than merely tidy. That file owns the LEG — the row, when it
// opens, when it closes and on what evidence. leg_banners.go owns the ALERT,
// which is now the fallback surface for a phone with no card. This file owns
// the CARD, which is the primary one.
//
// Each of the three deliveries here is CLAIMED before it is performed, because
// they are separate deliveries with separate failure modes: a push-to-start can
// fail while its banner succeeded, and each must be retryable without
// re-sending the other.

// startLegActivity push-to-starts the card on every registered phone.
//
// A LEG WITH NO REGISTRATIONS IS NOT A FAILURE and is not retried: the claim is
// taken whatever the fan-out finds. That is the "a leg that never got a token
// registration still gets its pushes" rule from the other side — the banner has
// already gone out, and re-attempting a push-to-start every frame for a trip
// whose participants are all on the web would be an unbounded loop over an
// empty set.
func (s *Service) startLegActivity(ctx context.Context, leg Leg, audience TripAudience) {
	if s.activities == nil {
		return
	}
	claimed, err := s.legs.ClaimLegActivityStart(ctx, leg.ID)
	if err != nil {
		s.logger.Warn("trips: leg activity-start claim failed",
			slog.String("leg_id", leg.ID), slog.String("error", err.Error()))
		return
	}
	if !claimed {
		return
	}
	s.activities.StartLeg(ctx, s.legContext(ctx, leg, audience, tripStatusEnroute, nil))
}

// endLegActivity delivers the alerting update and the `end`.
func (s *Service) endLegActivity(ctx context.Context, leg Leg, audience TripAudience, status string, at time.Time) {
	if s.activities == nil {
		return
	}
	claimed, err := s.legs.ClaimLegActivityEnd(ctx, leg.ID)
	if err != nil {
		s.logger.Warn("trips: leg activity-end claim failed",
			slog.String("leg_id", leg.ID), slog.String("error", err.Error()))
		return
	}
	if !claimed {
		return
	}
	s.activities.EndLeg(ctx, s.legContext(ctx, leg, audience, status, &at))
}

// legContext assembles the card's content-state inputs.
//
// The ETA is NOT carried here and is nil on every call. The card gets its
// arrival time from the ticker path (updateLeg), which reads it from the frame
// that prompted the update; a start or an end is triggered by a TRANSITION, and
// the honest answer at either instant is that we have not computed one — an
// absent `eta` renders a card with no time rather than one with a wrong time,
// which is MYR-194's rule about never inventing a number.
func (s *Service) legContext(
	ctx context.Context,
	leg Leg,
	audience TripAudience,
	status string,
	asOf *time.Time,
) push.TripLegContext {
	tc := push.TripLegContext{
		LegID:       leg.ID,
		TripID:      leg.TripID,
		VehicleID:   leg.VehicleID,
		TripName:    s.tripName(ctx, leg.TripID),
		VehicleName: s.vehicleName(ctx, audience.VehicleID),
		Destination: leg.DestinationName,
		Status:      status,
	}
	if asOf != nil {
		tc.AsOf = *asOf
	}
	return tc
}

// CatchUpLegActivity raises the card for a leg that is ALREADY UNDER WAY, for
// the one person who has just registered a push-to-start token.
//
// ⚠ THIS IS THE FIX FOR "NO CARD FOR ANYBODY, EVER" (MYR-612). A leg's Live
// Activity is push-to-start, and the fan-out runs ONCE, at the instant the leg
// opens, over whatever tokens are registered then. Registering is what a phone
// does when the `trip_leg_started` push wakes it — which is necessarily AFTER
// the leg opened. On 2026-09-08 the only participant's token was written at
// 03:40:27 for a leg that opened at 03:40:24, three seconds too late; the trip
// ran for the rest of the evening with `go_live_activities` empty.
//
// So the registration itself is now an occasion to send. It is safe to call on
// every registration, including the overwhelming majority that arrive with no
// leg open: the store's per-(device, leg) claim is the idempotency, and a trip
// with no open leg reads one row and sends nothing.
//
// BEST-EFFORT BY CONSTRUCTION. It returns nothing, because it is called after
// the registration has committed and there is no failure the caller could act
// on — the token is stored either way, and the next leg's fan-out will use it.
func (s *Service) CatchUpLegActivity(ctx context.Context, tripID, userID string) {
	if s.activities == nil || tripID == "" || userID == "" {
		return
	}
	legs, err := s.legs.OpenLegsForTrip(ctx, tripID)
	if err != nil {
		s.logger.Warn("trips: catch-up open-leg lookup failed",
			slog.String("trip_id", tripID), slog.String("error", err.Error()))
		return
	}
	if len(legs) == 0 {
		// The ordinary case: a trip whose car is parked, or between legs.
		return
	}
	// At most one — the partial unique index says so — but taken as a list
	// because the teardown path must handle whatever it finds rather than
	// assume the invariant it is cleaning up after.
	leg := legs[0]

	audience, err := s.trips.TripAudienceFor(ctx, tripID)
	if err != nil {
		s.logger.Warn("trips: catch-up audience lookup failed; no card raised",
			slog.String("leg_id", leg.ID), slog.String("error", err.Error()))
		return
	}

	// THE CONTENT STATE CARRIES NO ETA, exactly as the leg-open fan-out's does
	// and for the same reason: this instant is a REGISTRATION, not a frame, and
	// the honest answer here is that we have not computed one. The next frame
	// that earns a card refresh puts a time on it, and an absent `eta` renders
	// a card with no time rather than one with a wrong time — MYR-194's rule.
	s.activities.StartLegForUser(ctx, s.legContext(ctx, leg, audience, tripStatusEnroute, nil), userID)
}
