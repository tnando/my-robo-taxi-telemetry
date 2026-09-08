package trips

import (
	"context"
	"fmt"
	"log/slog"
	"time"
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
// THE PROBE IS SCOPED TO THIS TRIP (MYR-612 review). The merge window is a
// couple of minutes and a car very often begins its next trip inside one — the
// owner ends a road trip on the drive and starts the next before setting off
// again, to the same place, on the same car. A cross-trip resume attaches the
// row to the trip it already belonged to while every delivery addresses THIS
// trip's audience: a card for the wrong people, no leg at all in the right
// trip's history, and nothing this trip's detector could ever close.
//
// A RESUME FAILURE IS NOT FATAL and is deliberately swallowed into the ordinary
// path: the repair is an improvement on inserting a row, never a precondition
// for it, and a leg that opens as a second row is a cosmetic fault where a leg
// that never opens is a silent one.
func (s *Service) startOrResumeLeg(
	ctx context.Context, tv TripVehicle, destination string, at time.Time,
) (Leg, bool, error) {
	notBefore := at.Add(-s.cfg.LegMergeWindow)
	leg, resumed, err := s.legs.ResumeRecentLeg(ctx, tv.TripID, tv.VehicleID, destination, notBefore)
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
//
// IT REPORTS WHETHER THE ROW ACTUALLY CLOSED, because the detector's per-car
// memory of this ending must not be written until it did — see closeLegNow.
func (s *Service) closeLeg(ctx context.Context, leg Leg, audience TripAudience, arrived bool) bool {
	at := s.now()
	if err := s.legs.EndLeg(ctx, leg.ID, at, arrived); err != nil {
		s.logger.Error("trips: closing a leg failed; its card may be stranded",
			slog.String("leg_id", leg.ID), slog.String("error", err.Error()))
		return false
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
	return true
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
