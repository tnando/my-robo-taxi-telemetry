package trips

import (
	"context"
	"fmt"
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
	if !s.windowStillOpen(ctx, tv) {
		return
	}
	leg, resumed, err := s.startOrResumeLeg(ctx, tv, destination, at)
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

	// BOTH PATHS RUN THE SAME TWO DELIVERIES, and that is what makes a resume
	// safe rather than a special case. Each is CLAIMED first, so a resumed leg
	// re-sends only what its ending actually undid: the start banner's stamp
	// survives a resume and the banner is not repeated, while a card that was
	// ENDED had its push-to-start claim released and is raised again — which is
	// exactly right, because that card is gone from the lock screen.
	s.announceLegStarted(ctx, leg, audience)
	s.startLegActivity(ctx, leg, audience)

	s.logger.Info("trip leg opened",
		slog.Bool("resumed", resumed),
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

// startOrResumeLeg gets the leg this journey belongs to.
//
// IT ASKS "IS THIS THE SAME JOURNEY?" BEFORE IT ASKS FOR A NEW ONE (MYR-612).
// A car that has just closed a leg WITHOUT ARRIVING and is now setting off
// again, within LegMergeWindow, for the SAME destination has not started a
// second journey — it has had one interrupted by something the detector could
// not see: a restart between two frames, two servers during a rolling deploy, a
// destination-clear debounce that expired one frame before the name came back.
// Inserting a second row for it produces a second `trip_leg_started` banner, a
// second card, and a trip history claiming the car drove to one hotel twice.
//
// A RESUME FAILURE IS NOT FATAL and is deliberately swallowed into the ordinary
// path: the repair is an improvement on inserting a row, never a precondition
// for it, and a leg that opens as a second row is a cosmetic fault where a leg
// that never opens is a silent one.
func (s *Service) startOrResumeLeg(
	ctx context.Context, tv TripVehicle, destination string, at time.Time,
) (Leg, bool, error) {
	notBefore := at.Add(-s.cfg.LegMergeWindow)
	leg, resumed, err := s.legs.ResumeRecentLeg(ctx, tv.VehicleID, destination, notBefore)
	switch {
	case err != nil:
		s.logger.Warn("trips: leg resume probe failed; opening a new leg",
			slog.String("vehicle_id", tv.VehicleID),
			slog.String("error", err.Error()))
	case resumed:
		return leg, true, nil
	}

	leg, err = s.legs.StartLeg(ctx, tv.TripID, tv.VehicleID, destination, at)
	if err != nil {
		return Leg{}, false, fmt.Errorf("trips.startOrResumeLeg(trip=%s): %w", tv.TripID, err)
	}
	return leg, false, nil
}

// windowStillOpen re-asks the database whether this car is inside THIS trip's
// window, immediately before a leg is written.
//
// THE CANDIDATE SNAPSHOT IS DELIBERATELY STALE and the detector's own doc says
// why: it is refreshed at most once per CandidateTTL, and on a failed refresh
// it is served for up to four TTLs more. That is the right trade for the
// per-frame path, which must not pay for a query per frame — but it is the
// wrong basis for a WRITE. A minute-old snapshot cannot know that the owner
// tapped "End trip" thirty seconds ago, and opening a leg on a window that has
// closed pushes a Live Activity and a banner naming the car and its
// destination to people whose access was revoked with the window. The
// detector's own comment already states this as the reason the stale path
// fails towards detecting nothing; this is the same rule applied at the one
// moment the snapshot is acted upon rather than merely consulted.
//
// It costs one indexed read per LEG EDGE — a handful per car per day, not one
// per frame — which is why the confirmation is here and not in the frame path.
//
// AN ERROR REFUSES THE LEG. Failing open would mean "we could not check, so we
// pushed anyway", which is the one direction this feature is not allowed to
// fail in; a leg refused on a database blip is re-opened by the next frame
// that finds the car still driving with a destination, because the open
// condition is a state and not an edge the detector can only see once.
func (s *Service) windowStillOpen(ctx context.Context, tv TripVehicle) bool {
	tripID, err := s.trips.ActiveTripForVehicle(ctx, tv.VehicleID)
	if err != nil {
		s.logger.Warn("trips: could not confirm the window before opening a leg; refusing it",
			slog.String("trip_id", tv.TripID),
			slog.String("vehicle_id", tv.VehicleID),
			slog.String("error", err.Error()))
		return false
	}
	if tripID == tv.TripID {
		return true
	}
	// Either the window closed under the snapshot, or the car is now inside a
	// DIFFERENT trip's window. Both mean the leg would have been attributed to
	// the wrong trip, and the second is the one the create endpoint's overlap
	// refusal makes rare rather than impossible (a window ending while the
	// next begins).
	s.logger.Info("trips: the snapshot's window is no longer this car's; leg not opened",
		slog.String("snapshot_trip_id", tv.TripID),
		slog.String("vehicle_id", tv.VehicleID),
		slog.Bool("now_in_a_window", tripID != ""))
	return false
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
