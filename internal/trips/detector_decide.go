package trips

import (
	"context"
	"fmt"
	"log/slog"
	"time"
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
	leg, err := d.openLegFor(ctx, state, tv.VehicleID, f.at)
	if err != nil {
		d.logger.Warn("trips: open-leg lookup failed",
			slog.String("vehicle_id", tv.VehicleID), slog.String("error", err.Error()))
		return
	}

	if !leg.Open() {
		state.endLeg()
		// A PENDING CLEAR WITH NO OPEN LEG IS ALREADY SETTLED (MYR-612).
		//
		// The debounce exists to protect an OPEN leg from one delta that
		// happened to omit the destination name; with no leg open there is
		// nothing to protect and nothing left to confirm it either — branch 2
		// below is the only place that calls confirmClear, and it is
		// unreachable from here. Left pending, `destination` keeps naming a
		// place the driver cancelled while parked, and the next time the car
		// pulls out it reads as "driving with a destination" although no route
		// is set: a PHANTOM leg, with a banner and a card, for a journey
		// nobody planned.
		if state.clearPending() {
			state.confirmClear()
			return
		}
		if state.driving && state.destination != "" && (!drivingBefore || destBefore == "") {
			edgeCtx, cancel := context.WithTimeout(ctx, d.cfg.EdgeTimeout)
			defer cancel()
			d.svc.openLeg(edgeCtx, tv, state.destination, f.at)
			state.forgetLegRead()
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
	//
	// ⚠ THE AUDIENCE IS NO LONGER READ HERE (MYR-612). It used to be fetched
	// before the three checks, on EVERY FRAME of every car inside an open
	// window — up to one query per second per car, on the single bus goroutine,
	// with no timeout of its own — and it is used by exactly one outcome in
	// several hundred: a leg EDGE. Together with the open-leg read next to it
	// that was two unbounded statements per frame; one car on one road trip
	// held two pool connections busy for four minutes, which is how a JWT
	// existence probe on an unrelated request came to time out and answer 401
	// (see internal/auth/user_existence_cache.go). It is read inside
	// closeLegNow instead, at the edge that actually needs it.
	//
	// Computed BEFORE the dwell is folded in, because `arrivedAt` mutates the
	// track and this is a pure question about this frame alone.
	atDestination := state.inRadius(f, d.cfg)

	// 1. ARRIVAL — the strongest claim, and the only one that fires
	//    `trip_leg_arrived`. Latched so the twenty further qualifying frames
	//    that arrive while the car sits there do nothing.
	if !state.arrivalLatched && state.arrivedAt(f, d.cfg) {
		state.arrivalLatched = true
		d.closeLegNow(ctx, tv, state, leg, true)
		return
	}

	// 2. THE ROUTE WAS CLEARED — ONCE THE CLEAR IS CONFIRMED (MYR-612).
	//
	//    THIS BRANCH USED TO FIRE ON THE FIRST FRAME WITH AN EMPTY NAME, and
	//    that is the bug the issue was raised for. Tesla streams deltas, and a
	//    car four minutes into a leg to "Element by Marriott Sedona" sent a
	//    frame whose destination name was present-but-empty while its
	//    `minutesToArrival` still read 98 and the dash still showed the place.
	//    The leg closed at 03:40:22 and re-opened at 03:40:24 — two legs for
	//    one journey, two start banners, two push-to-start fan-outs, and any
	//    card raised for the first ended as `completed` while the car drove on.
	//
	//    The clear is now PENDING until park, a sustained absence of both the
	//    name and any arrival estimate, or arrival evidence (branch 1, above)
	//    settles it. See vehicleState.clearConfirmed.
	if state.clearPending() {
		if !state.clearConfirmed(f, d.cfg, atDestination) {
			// Held open. The card is deliberately NOT refreshed while the
			// route is in doubt: the honest content-state is the one already
			// on the lock screen, and its stale-date keeps it honest.
			return
		}
		state.confirmClear()
		d.closeLegNow(ctx, tv, state, leg, false)
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
	//    PARKED IS NOT "STOPPED ON THIS FRAME" (MYR-612 review). `!driving`
	//    alone says only that the last frame reported a speed at or under 1
	//    mph, which a car waiting at a junction reports for twenty seconds at a
	//    time — so this branch used to close a leg at the first red light away
	//    from the destination, mid-route, as `completed`. vehicleState.parked
	//    is the claim this branch was always making in prose: the driver put it
	//    in P, or it has not moved for a whole LegClearGrace.
	//
	//    The residual case is a car that parks at its destination and then goes
	//    completely silent, with not even a MYR-394 REST poll frame to satisfy
	//    the dwell. Its leg stays open until the window closes and is then
	//    settled as `completed`. That is the honest answer — nothing ever
	//    proved it stayed — and it is the same dependency internal/arrival has
	//    on the same poller for the same reason.
	//    `drivingBefore` IS NOT PART OF THE TEST any more, and could not be:
	//    it is true on exactly the FIRST stopped frame, so pairing it with a
	//    stop that has to be SUSTAINED asks for two things that can never hold
	//    at once. A park is a state the detector re-observes on every frame
	//    until it acts on it, which is the same shape every other closing
	//    condition here has.
	if !atDestination && state.parked(f, d.cfg) {
		d.closeLegNow(ctx, tv, state, leg, false)
		return
	}

	// Still under way. Refresh the card only when this frame's arrival estimate
	// has earned a push — see vehicleState.dueForCard.
	if minutes := etaMinutesFrom(f); state.dueForCard(minutes, f.at) {
		edgeCtx, cancel := context.WithTimeout(ctx, d.cfg.EdgeTimeout)
		defer cancel()
		d.svc.updateLeg(edgeCtx, leg, f, minutes)
	}
}

// openLegFor answers "does this car have an open leg", from a SHORT-LIVED CACHE
// rather than from a statement per frame.
//
// MYR-612. This read is the detector's only per-frame database call now that
// the audience has moved to the edges, and at one frame per second per car it
// was still a sustained query stream on the bus goroutine with no timeout of
// its own. The answer changes at most twice per journey, so serving it from
// memory for LegReadTTL costs a few seconds of latency on an edge — against a
// twenty-second dwell and a twenty-second card floor, invisible — and takes the
// steady-state cost to roughly one query per car per TTL.
//
// EVERY WRITE INVALIDATES IT (forgetLegRead), so the frame after an open or a
// close reads the truth rather than the picture that prompted the write.
//
// THE CLOCK IS THE FRAME'S, and the window is checked from BOTH sides. A frame
// older than the cached read — which the MYR-394 REST poller legitimately
// produces — falls through to a fresh read rather than being treated as
// arbitrarily recent, so a burst of backlogged frames cannot hold a stale
// answer past its TTL. Failing towards "read it again" is the cheap direction.
func (d *Detector) openLegFor(ctx context.Context, state *vehicleState, vehicleID string, at time.Time) (Leg, error) {
	if fresh := at.Sub(state.legReadAt); !state.legReadAt.IsZero() && fresh >= 0 && fresh < d.cfg.LegReadTTL {
		return state.legRead, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	leg, err := d.svc.legs.OpenLegForVehicle(readCtx, vehicleID)
	if err != nil {
		return Leg{}, fmt.Errorf("trips.openLegFor(vehicle=%s): %w", vehicleID, err)
	}
	state.legRead, state.legReadAt = leg, at
	return leg, nil
}

// closeLegNow reads the audience and ends the leg.
//
// THE AUDIENCE READ LIVES HERE, at the one outcome that uses it, rather than on
// every frame — see the note in decide. A failed read DEFERS the edge exactly as
// it did before: the leg stays open and the next qualifying frame tries again,
// because every closing condition is a STATE the detector can re-observe rather
// than an edge it sees once.
func (d *Detector) closeLegNow(ctx context.Context, tv TripVehicle, state *vehicleState, leg Leg, arrived bool) {
	readCtx, cancelRead := context.WithTimeout(ctx, d.cfg.Timeout)
	audience, err := d.svc.trips.TripAudienceFor(readCtx, tv.TripID)
	cancelRead()
	if err != nil {
		d.logger.Warn("trips: leg audience lookup failed; deferring the leg edge",
			slog.String("leg_id", leg.ID), slog.String("error", err.Error()))
		return
	}

	edgeCtx, cancel := context.WithTimeout(ctx, d.cfg.EdgeTimeout)
	defer cancel()
	d.svc.closeLeg(edgeCtx, leg, audience, arrived)
	state.forgetLegRead()
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
func (s *Service) updateLeg(ctx context.Context, leg Leg, f fix, minutes *int) {
	if s.activities == nil || minutes == nil {
		return
	}
	// NO AUDIENCE ARGUMENT (MYR-612). The only thing legContext ever read from
	// it here was the vehicle id, which the LEG already carries — the leg row's
	// `vehicle_id` and the audience's are the same column by construction — so
	// requiring one meant a per-frame audience query for a projection that
	// never used the rest of it.
	tc := s.legContext(ctx, leg, TripAudience{VehicleID: leg.VehicleID}, tripStatusEnroute, &f.at)
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
