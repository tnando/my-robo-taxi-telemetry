package trips

import (
	"context"
	"log/slog"
)

// THE EDGES one frame produced, and what each of them does.
//
// Split from detector.go so both stay inside the 300-line cap, along the seam
// the two halves already had: that file owns the LIFECYCLE — the subscription,
// the per-frame plumbing, the candidate snapshot and the per-car memory — and
// this one owns the DECISION. The decision is the part a false leg or a missed
// arrival comes out of, and it reads better with nothing else on the page.

// decide acts on the edges one frame produced.
//
// THE OPEN CONDITION IS A TRANSITION INTO "driving, with a destination", from
// EITHER SIDE — the car started moving with a route already set, or it was
// already moving and a route appeared. The second case is the common one (a
// driver sets the destination on the dash after pulling out) and is exactly
// what a drive-start event could never have expressed; see the package doc.
//
// A leg that is already open is not re-opened: StartLeg is idempotent against
// the one-open-leg-per-trip index, and a car that RE-ROUTES mid-leg keeps its
// leg rather than getting a second card for one journey.
func (d *Detector) decide(
	ctx context.Context, tv TripVehicle, state *vehicleState, f fix, drivingBefore bool, destBefore string,
) {
	leg, err := d.svc.legs.OpenLegForVehicle(ctx, tv.VehicleID)
	if err != nil {
		d.logger.Warn("trips: open-leg lookup failed",
			slog.String("vehicle_id", tv.VehicleID), slog.String("error", err.Error()))
		return
	}

	underway := state.driving && state.destination != ""
	if !leg.Open() {
		state.endLeg()
		if underway && (!drivingBefore || destBefore == "") {
			d.svc.openLeg(ctx, tv, state.destination, f.at)
		}
		return
	}
	// The per-leg fields (the arrival latch, the dwell, the card throttle)
	// belong to THIS leg. Re-pointing them here is what stops a latch set on
	// the previous leg from disabling this one's arrival branch — see
	// vehicleState.beginLeg.
	state.beginLeg(leg.ID)

	// A leg IS open. Three things can close it, and they are checked in the
	// order of how much they assert.
	audience, audErr := d.svc.trips.TripAudienceFor(ctx, tv.TripID)
	if audErr != nil {
		d.logger.Warn("trips: leg audience lookup failed; deferring the leg edge",
			slog.String("leg_id", leg.ID), slog.String("error", audErr.Error()))
		return
	}

	// Computed BEFORE the dwell is folded in, because `arrivedAt` mutates the
	// track and this is a pure question about this frame alone.
	atDestination := state.inRadius(f, d.cfg)

	// 1. ARRIVAL — the strongest claim, and the only one that fires
	//    `trip_leg_arrived`. Latched so the twenty further qualifying frames
	//    that arrive while the car sits there do nothing.
	if !state.arrivalLatched && state.arrivedAt(f, d.cfg) {
		state.arrivalLatched = true
		d.svc.closeLeg(ctx, leg, audience, true)
		return
	}

	// 2. THE ROUTE WAS CLEARED. The driver cancelled navigation; the car may
	//    still be moving. The leg is over as a leg — there is no longer a place
	//    it is going — and it ended without evidence.
	if state.destination == "" {
		d.svc.closeLeg(ctx, leg, audience, false)
		return
	}

	// 3. THE CAR PARKED SHORT of its destination. `completed`, not `arrived`:
	//    it stopped somewhere, and nothing says it was the right somewhere.
	//
	//    THE `!atDestination` GUARD IS LOAD-BEARING AND WAS THE FIRST BUG THIS
	//    DETECTOR'S TESTS FOUND. A car that ARRIVES also stops, so without it
	//    every successful arrival was closed as `completed` by this branch on
	//    the very first stopped frame — one second into a dwell that needs
	//    twenty — and the arrival could never fire. A stop INSIDE the radius is
	//    the beginning of an arrival, not the end of a leg; the dwell decides
	//    which, and until it does the leg stays open.
	//
	//    The residual case is a car that parks at its destination and then goes
	//    completely silent, with not even a MYR-394 REST poll frame to satisfy
	//    the dwell. Its leg stays open until the window closes and is then
	//    settled as `completed`. That is the honest answer — nothing ever
	//    proved it stayed — and it is the same dependency internal/arrival has
	//    on the same poller for the same reason.
	if !state.driving && drivingBefore && !atDestination {
		d.svc.closeLeg(ctx, leg, audience, false)
		return
	}

	// Still under way. Refresh the card only when this frame's arrival estimate
	// has earned a push — see vehicleState.dueForCard.
	if minutes := etaMinutesFrom(f); state.dueForCard(minutes, f.at) {
		d.svc.updateLeg(ctx, leg, audience, f, minutes)
	}
}

// updateLeg refreshes a running card mid-leg.
//
// THE THROTTLE IS THE CALLER'S (vehicleState.dueForCard) rather than this
// function's, because it is per-VIN state and this is a stateless projection.
// What matters is that it exists: Apple throttles high-frequency Activity
// pushes by budget and a car streams up to once per second, so a refresh has to
// earn its push — the arrival minute must have moved, and a floor interval must
// have passed. The card's 3-minute stale-date is what keeps it honest between
// pushes.
//
// It lives on Service rather than Detector because the leg lifecycle belongs
// together, and because a future ticker could call it on the same terms.
func (s *Service) updateLeg(ctx context.Context, leg Leg, audience TripAudience, f fix, minutes *int) {
	if s.activities == nil || minutes == nil {
		return
	}
	tc := s.legContext(ctx, leg, audience, tripStatusEnroute, &f.at)
	tc.ETAMinutes = minutes
	s.activities.UpdateLeg(ctx, tc)
}

// etaMinutesFrom reads the car's own arrival estimate off a frame.
//
// NIL WHEN THE CAR DOES NOT SAY, never a computed guess. There is no route
// solver in this service, and MYR-194 rules out inventing a number in as many
// words: an absent `eta` renders a card with no time, which is a first-class
// state the client already handles.
func etaMinutesFrom(f fix) *int {
	if f.minutesToGo == nil {
		return nil
	}
	m := int(*f.minutesToGo + 0.5)
	if m < 0 {
		return nil
	}
	return &m
}
