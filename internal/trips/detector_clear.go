package trips

import "time"

// WAS THE ROUTE REALLY CLEARED, AND HAS THE CAR REALLY PARKED? The two rules
// that let a leg end WITHOUT an arrival.
//
// Split from detector_state.go under the 300-line cap, along the seam the
// MYR-612 review exposed: three separate defects lived in these forty lines —
// a clear that could never be confirmed once any frame carried an estimate, a
// "park" that fired at a red light, and a missing `!atDestination` guard that
// closed an arrival as `completed` one second into a twenty-second dwell — and
// every one of them is a rule about ABSENCE. Absence is the hard half of this
// detector and it reads better on its own page.
//
// Its sibling, detector_arrival.go, owns the rule about presence.

// clearConfirmed reports whether a PENDING destination clear has become a real
// one — the MYR-612 debounce, stated as the three things that are allowed to
// end a leg on an absent destination.
//
//	PARK       the car PARKED — gear P, or a stop sustained for the grace.
//	           Nothing needs debouncing: a parked car with no route is not on
//	           its way anywhere, and the park-short branch would reach the same
//	           verdict a frame later anyway.
//	SUSTAINED  LegClearGrace has passed since the name went, AND no arrival
//	           estimate has been reported in that time. Both, not either: a car
//	           still saying how long it has to go still has somewhere to be, and
//	           that is exactly the frame sequence the incident produced.
//	(ARRIVAL)  handled before this is ever reached — it is the strongest claim
//	           and the only one that fires `trip_leg_arrived`.
//
// ⚠ THE PARK RUNG CARRIES `!atDestination`, exactly as decide's park-short
// branch does and for the same reason. A car that ARRIVES also stops, so
// without the guard a route cleared on arrival — which is what a dash does
// when it reaches the place — closed the leg as `completed` one second into a
// twenty-second dwell, and `trip_leg_arrived` was never sent. A stop inside
// the radius is the beginning of an arrival, not the end of a leg.
func (v *vehicleState) clearConfirmed(f fix, cfg Config, atDestination bool) bool {
	if v.destClearedAt.IsZero() {
		return false
	}
	if !atDestination && v.parked(f, cfg) {
		return true
	}
	if f.at.Sub(v.destClearedAt) < cfg.LegClearGrace {
		return false
	}
	// THE RULE IS "NO ESTIMATE RECENTLY", NOT "NO ESTIMATE SINCE THE CLEAR".
	//
	// This used to carry a second conjunct, `etaSeenAt.Before(destClearedAt)`,
	// and it made a confirmed clear IMPOSSIBLE on the very sequence the
	// debounce exists for. The incident's own frame carried the empty name and
	// `minutesToArrival = 98` TOGETHER, so the two stamps were equal and
	// `Before` was false; any later frame carrying an estimate pushed etaSeenAt
	// past the clear for good. A genuinely cancelled route whose car had ever
	// reported an estimate at or after the clear could then never close its leg
	// — the card would sit on the lock screen until the window itself lapsed.
	//
	// Staleness is the whole question, and it is asked of the estimate alone: a
	// car that has not said how long it has to go for a full grace has stopped
	// claiming to be going anywhere. A zero etaSeenAt (a car that never
	// reported one) is arbitrarily stale, which is the right answer for it.
	return f.at.Sub(v.etaSeenAt) >= cfg.LegClearGrace
}

// parked reports whether the car has actually PARKED, as against merely having
// reported a speed at or under the stopped threshold on this one frame.
//
// ⚠ `!driving` IS NOT PARKED, and using it as though it were is what made the
// MYR-612 debounce evaporate at the first red light: a car waiting at a
// junction reports 0 mph for twenty seconds at a time, and one such frame after
// a delta that happened to omit the destination name closed the leg on the
// spot — the exact outcome the debounce was added to prevent.
//
// Two things count, and both are claims about the car rather than about one
// frame: the driver put it in P, or it has not moved for a whole
// LegClearGrace. The second is deliberately the same duration as the grace the
// SUSTAINED rung waits out — a stop long enough to settle a clear on its own is
// a stop no longer than the wait it is short-cutting.
func (v *vehicleState) parked(f fix, cfg Config) bool {
	if f.gear == gearPark {
		return true
	}
	if v.driving || v.stoppedSince.IsZero() {
		return false
	}
	return f.at.Sub(v.stoppedSince) >= cfg.LegClearGrace
}

// clearPending reports whether the car is inside the debounce window.
func (v *vehicleState) clearPending() bool { return !v.destClearedAt.IsZero() }

// confirmClear finally forgets the route. Called only when clearConfirmed said
// so, immediately before the leg is closed.
func (v *vehicleState) confirmClear() {
	v.destination = ""
	v.destClearedAt = time.Time{}
	v.hasDest, v.destLat, v.destLng = false, 0, 0
	v.stillSince = time.Time{}
	v.arrivalLatched = false
	v.lastCardETA, v.lastCardPush = nil, time.Time{}
}
