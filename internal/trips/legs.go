package trips

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/push"
)

// The leg lifecycle: what HAPPENS when a leg opens and closes, as against what
// NOTICES it (detector.go) and when the window itself moves (transitions.go).
//
// Every step is CLAIMED before it is performed. A leg has four independent
// deliveries — the start banner, the push-to-start, the arrival banner and the
// end pair — and each carries its own stamp, because they are four separate
// deliveries with four separate failure modes: an alert can succeed while a
// push-to-start fails, and each must be retryable without re-sending the other.

// openLeg records a new leg and performs its two deliveries.
//
// THE ORDER IS BANNER FIRST, CARD SECOND, and it is the reverse of the ending's
// order for a reason that is about what happens when only one of them works. A
// banner with no card is a complete, if plain, experience — the person is told
// the car set off and where to. A card with no banner is fine too. But the card
// is the surface that keeps costing something if it is wrong (it sits on a lock
// screen for the whole leg), so it goes second: if the process dies between the
// two, the participant is left with a banner and no card, which the next leg
// corrects, rather than a card nobody was told about.
func (s *Service) openLeg(ctx context.Context, tv TripVehicle, destination string, at time.Time) {
	leg, err := s.legs.StartLeg(ctx, tv.TripID, tv.VehicleID, destination, at)
	if err != nil {
		s.logger.Error("trips: opening a leg failed",
			slog.String("trip_id", tv.TripID),
			slog.String("vehicle_id", tv.VehicleID),
			slog.String("error", err.Error()))
		return
	}
	if leg.ID == "" {
		return
	}

	audience, err := s.trips.TripAudienceFor(ctx, tv.TripID)
	if err != nil {
		s.logger.Warn("trips: leg audience lookup failed; the leg is open but silent",
			slog.String("leg_id", leg.ID),
			slog.String("error", err.Error()))
		return
	}

	s.announceLegStarted(ctx, leg, audience)
	s.startLegActivity(ctx, leg, audience)

	s.logger.Info("trip leg opened",
		slog.String("trip_id", leg.TripID),
		slog.String("leg_id", leg.ID),
		slog.String("vehicle_id", leg.VehicleID),
		// The DESTINATION IS NOT LOGGED. It is P1 — a place a car actually
		// drove to (data-classification.md §1.18) — and its presence is the
		// only part an operator needs.
		slog.Bool("has_destination", leg.DestinationName != ""),
		slog.Int("audience", len(audience.everyone())),
	)
}

// announceLegStarted fires `trip_leg_started` once per leg.
//
// TO EVERYONE, owner included — unlike the three lifecycle pushes, which go to
// participants only. The owner is on the leg card by explicit product decision,
// and a card with no banner behind it would make the driving party the one
// person in the feature who is never told anything.
func (s *Service) announceLegStarted(ctx context.Context, leg Leg, audience TripAudience) {
	claimed, err := s.legs.ClaimLegStartedPush(ctx, leg.ID)
	if err != nil {
		s.logger.Warn("trips: leg-start push claim failed",
			slog.String("leg_id", leg.ID), slog.String("error", err.Error()))
		return
	}
	if !claimed {
		return
	}
	s.notify(ctx, push.TripPush{
		TripID:          leg.TripID,
		VehicleID:       leg.VehicleID,
		Event:           push.TripEventLegStarted,
		LegID:           leg.ID,
		DestinationName: leg.DestinationName,
		UserIDs:         audience.everyone(),
	})
}

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

// closeLeg ends a leg: records the verdict, fires `trip_leg_arrived` when there
// was evidence, and ends the card.
//
// THE ORDER IS CARD FIRST, BANNER SECOND — the reverse of opening — because
// here the card is the thing that is actively WRONG until it is dealt with. A
// card left saying "heading to the Grand Canyon" after the car parked there is
// a lie on a lock screen for hours; a missing arrival banner is a silence.
//
// `arrived` false covers three different endings — the car parked somewhere
// else, its route was cleared, or the window closed underneath it — and they
// are deliberately not distinguished. What the surfaces need to know is whether
// the car REACHED the place it said it was going, and all three answers are no.
func (s *Service) closeLeg(ctx context.Context, leg Leg, audience TripAudience, arrived bool) {
	at := s.now()
	if err := s.legs.EndLeg(ctx, leg.ID, at, arrived); err != nil {
		s.logger.Error("trips: closing a leg failed; its card may be stranded",
			slog.String("leg_id", leg.ID), slog.String("error", err.Error()))
		return
	}

	status := tripStatusCompleted
	if arrived {
		status = tripStatusArrived
	}
	s.endLegActivity(ctx, leg, audience, status, at)

	if arrived {
		s.announceLegArrived(ctx, leg, audience)
	}

	s.logger.Info("trip leg closed",
		slog.String("trip_id", leg.TripID),
		slog.String("leg_id", leg.ID),
		slog.String("status", status),
		slog.Duration("duration", at.Sub(leg.StartedAt)),
	)
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

// announceLegArrived fires `trip_leg_arrived`, ONLY on evidence.
func (s *Service) announceLegArrived(ctx context.Context, leg Leg, audience TripAudience) {
	claimed, err := s.legs.ClaimLegArrivedPush(ctx, leg.ID)
	if err != nil {
		s.logger.Warn("trips: leg-arrival push claim failed",
			slog.String("leg_id", leg.ID), slog.String("error", err.Error()))
		return
	}
	if !claimed {
		return
	}
	s.notify(ctx, push.TripPush{
		TripID:          leg.TripID,
		VehicleID:       leg.VehicleID,
		Event:           push.TripEventLegArrived,
		LegID:           leg.ID,
		DestinationName: leg.DestinationName,
		UserIDs:         audience.everyone(),
	})
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

// The three status values a leg's card carries. Mirrors the unexported
// constants in internal/push; stated here too because this package decides
// WHICH one applies and the contract's reasoning (a leg only ever sends these
// three of the eight) belongs beside that decision.
const (
	tripStatusEnroute   = "enroute"
	tripStatusArrived   = "arrived"
	tripStatusCompleted = "completed"
)
