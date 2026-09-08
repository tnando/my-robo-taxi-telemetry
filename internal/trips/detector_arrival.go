package trips

import "time"

// HAS THE CAR ARRIVED? The rules that answer it, and nothing else.
//
// Split from detector_state.go under the 300-line cap, along the seam that file
// already had. That one owns the per-car MEMORY — what a sequence of deltas
// adds up to, and which of it belongs to which leg. This one owns one QUESTION
// asked of that memory, and it is the question a false "your car arrived" comes
// out of: a sentence that cannot be taken back.
//
// Its sibling, detector_clear.go, owns the other closing question.

// arrivedAt folds one frame into the dwell and reports whether the car has now
// been stopped at its destination for the whole window.
//
// THE DISTANCE COMES FROM THE COORDINATE WHEN THE CAR STREAMS ONE, and from the
// car's own `milesToArrival` when it does not. That second source is the exact
// signal internal/arrival refuses, and refusing it there is right for the reason
// its package doc gives — the dash's target and the RIDE's target are different
// facts. ON A LEG THEY ARE THE SAME FACT BY CONSTRUCTION: a leg is defined as
// "the car is driving to the place the dash names", so the dash's distance to
// that place is the most direct evidence there is.
//
// STILLNESS IS STILL REQUIRED either way, which is what stops a leg "arriving"
// at a red light 60 m short of the destination, and the dwell is measured on
// FRAME timestamps so a burst of backlogged frames cannot fake it.
func (v *vehicleState) arrivedAt(f fix, cfg Config) bool {
	prev := v.prev
	if f.hasFix {
		snapshot := f
		v.prev = &snapshot
	}

	if !v.inRadius(f, cfg) {
		v.stillSince = time.Time{}
		return false
	}
	since, still := v.stillnessRun(f, prev, cfg)
	if !still {
		v.stillSince = time.Time{}
		return false
	}
	if v.stillSince.IsZero() {
		v.stillSince = since
	}
	// Not Before rather than After, so a zero dwell (tests) fires on the first
	// qualifying frame and a frame landing exactly on the boundary counts.
	return !f.at.Before(v.stillSince.Add(cfg.Dwell))
}

// inRadius reports whether this frame places the car at its destination.
func (v *vehicleState) inRadius(f fix, cfg Config) bool {
	if v.hasDest && f.hasFix {
		return haversineMeters(f.lat, f.lng, v.destLat, v.destLng) <= cfg.ArrivalRadiusMeters
	}
	if f.milesToGo != nil {
		return *f.milesToGo*metersPerMile <= cfg.ArrivalRadiusMeters
	}
	// No coordinate and no reported distance: nothing is claimed, which is the
	// direction every ambiguity in this detector resolves to.
	return false
}

// stillnessRun answers "is this car stopped, and if so since when", with the
// same three-rung ladder internal/arrival uses — a reported speed, then gear,
// then positional stillness across a bounded interval (MYR-563).
//
// The third rung matters here for the same reason it matters there and MORE: a
// car that parks STOPS STREAMING, so the frames that prove it stayed put are
// REST-backfill fixes carrying a location and nothing else.
func (v *vehicleState) stillnessRun(f fix, prev *fix, cfg Config) (time.Time, bool) {
	switch reportedMotion(f, cfg.StoppedSpeedMPH) {
	case motionStopped:
		// A reported stillness speaks for THIS instant only.
		return f.at, true
	case motionMoving:
		return time.Time{}, false
	case motionUnreported:
	}

	if prev == nil || !f.hasFix {
		return time.Time{}, false
	}
	gap := f.at.Sub(prev.at)
	if gap <= 0 || gap > cfg.MaxStillnessGap {
		return time.Time{}, false
	}
	if haversineMeters(prev.lat, prev.lng, f.lat, f.lng) > cfg.StillnessMeters {
		return time.Time{}, false
	}
	// Stillness proven across the interval, so the dwell may honestly count
	// from the EARLIER fix.
	return prev.at, true
}
