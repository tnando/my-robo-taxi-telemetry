package trips

import "time"

// THE NUMBERS, and the argument for each of them.
//
// Split from config.go under the 300-line cap, along the seam the two halves
// already had: that file declares WHAT each knob governs, this one says what
// value it takes and why that value and not another. They change for different
// reasons — a new knob there, a tuning decision here — and a reader arriving
// with "why sixty seconds?" wants only this page.

const (
	defaultSweepInterval  = 60 * time.Second
	defaultSweepLimit     = 200
	defaultCandidateTTL   = 15 * time.Second
	defaultCandidateLimit = 200

	// The arrival thresholds, copied from internal/arrival's defaults with
	// their reasoning intact:
	//
	//   80 m   about a city block's kerb frontage — absorbs a consumer GPS fix
	//          and the honest gap between a pin and where a car can stop.
	//   20 s   continuous stillness, the whole defence against a car that
	//          merely PASSES the destination: stopped at the lights outside it,
	//          queued behind a bus, waiting to turn.
	//   1 mph  not zero, because a stationary car's GPS-derived speed jitters
	//          around zero and demanding an exact 0.0 would restart the dwell
	//          at random.
	//   15 m   how far two consecutive fixes may be apart and still count as
	//          the car not having moved, when the frames report neither speed
	//          nor gear (the MYR-563 positional rung).
	//   90 s   the longest interval positional stillness may be inferred
	//          across; past it two identical fixes prove nothing.
	defaultArrivalRadiusMeters = 80.0
	defaultDwell               = 20 * time.Second
	defaultStoppedSpeedMPH     = 1.0
	defaultStillnessMeters     = 15.0
	defaultMaxStillnessGap     = 90 * time.Second

	// Sixty seconds, which is the number the incident argues for from both
	// sides: long enough that a burst of name-less deltas on a car that is
	// plainly still navigating cannot end a leg, and short enough that a driver
	// who really did cancel the route sees the card go within a minute.
	defaultLegClearGrace = 60 * time.Second

	// Twice the grace. A leg that closed despite the debounce closed for a
	// reason the debounce could not see, and two minutes is long enough to
	// cover a restart or a rolling deploy while being far shorter than any real
	// stop on a road trip — a car that parks, waits, and sets off again for the
	// same place after two minutes has genuinely made two journeys.
	defaultLegMergeWindow = 120 * time.Second

	// Half an hour, which is the number the client's screenshot argues for from
	// both sides: short enough that a genuine second departure for the same
	// place — a driver who arrives, waits, and sets off again — is still
	// announced, and long enough that no realistic flap can get a second
	// banner through. A trip going somewhere ELSE is never suppressed by it:
	// the destination is part of the key.
	defaultLegBannerWindow = 30 * time.Minute

	// Five seconds: a third of the candidate TTL, so a car entering or leaving
	// a leg is noticed within the same order of latency the candidate snapshot
	// already imposes, and far inside the dwell that decides an arrival.
	defaultLegReadTTL = 5 * time.Second

	defaultTimeout = 5 * time.Second

	// The whole per-frame path, edge included — see Config.FrameTimeout. The
	// same twenty seconds one trip's edge gets in a sweeper pass, because it
	// bounds the same work: a frame that opens or closes a leg does exactly
	// what a claimed trip's edge does.
	defaultFrameTimeout = 20 * time.Second

	// One trip's whole closing or opening edge, pushes included. Twenty
	// seconds is three times the budget its longest single statement gets and
	// a third of the interval, so a stalled edge is abandoned well before the
	// next pass and the 199 trips behind it still get theirs.
	defaultEdgeTimeout = 20 * time.Second

	// A whole sweep, not a statement — see Config.RevalidateTimeout. Half the
	// 60-second sweeper interval, so a pass that hangs is abandoned before the
	// next tick's edges arrive rather than making them queue behind it.
	defaultRevalidateTimeout = 30 * time.Second
)

// DefaultConfig returns the production settings with the feature ON.
func DefaultConfig() Config {
	return Config{
		Enabled:             true,
		SweepInterval:       defaultSweepInterval,
		SweepLimit:          defaultSweepLimit,
		CandidateTTL:        defaultCandidateTTL,
		CandidateLimit:      defaultCandidateLimit,
		LegClearGrace:       defaultLegClearGrace,
		LegMergeWindow:      defaultLegMergeWindow,
		LegBannerWindow:     defaultLegBannerWindow,
		LegReadTTL:          defaultLegReadTTL,
		ArrivalRadiusMeters: defaultArrivalRadiusMeters,
		Dwell:               defaultDwell,
		StoppedSpeedMPH:     defaultStoppedSpeedMPH,
		StillnessMeters:     defaultStillnessMeters,
		MaxStillnessGap:     defaultMaxStillnessGap,
		Timeout:             defaultTimeout,
		FrameTimeout:        defaultFrameTimeout,
		EdgeTimeout:         defaultEdgeTimeout,
		RevalidateTimeout:   defaultRevalidateTimeout,
	}
}

// withDefaults replaces zero-valued knobs with their defaults.
//
// Every zero but Dwell's would be a misconfiguration rather than a choice; the
// two halves say which is which.
func (c Config) withDefaults() Config {
	return c.withScheduleDefaults().withArrivalDefaults()
}

// withScheduleDefaults fills the CLOCK — every knob that is a duration or a
// batch size. Split from the arrival thresholds because they are two unrelated
// sets that happen to live on one struct, and one function filling both is a
// list long enough to trip the complexity gate.
func (c Config) withScheduleDefaults() Config {
	d := DefaultConfig()
	if c.SweepInterval <= 0 {
		c.SweepInterval = d.SweepInterval
	}
	if c.SweepLimit <= 0 {
		c.SweepLimit = d.SweepLimit
	}
	if c.CandidateTTL <= 0 {
		c.CandidateTTL = d.CandidateTTL
	}
	if c.CandidateLimit <= 0 {
		c.CandidateLimit = d.CandidateLimit
	}
	if c.LegClearGrace <= 0 {
		c.LegClearGrace = d.LegClearGrace
	}
	if c.LegMergeWindow <= 0 {
		c.LegMergeWindow = d.LegMergeWindow
	}
	if c.LegBannerWindow <= 0 {
		c.LegBannerWindow = d.LegBannerWindow
	}
	if c.LegReadTTL <= 0 {
		c.LegReadTTL = d.LegReadTTL
	}
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.FrameTimeout <= 0 {
		c.FrameTimeout = d.FrameTimeout
	}
	if c.EdgeTimeout <= 0 {
		c.EdgeTimeout = d.EdgeTimeout
	}
	if c.RevalidateTimeout <= 0 {
		c.RevalidateTimeout = d.RevalidateTimeout
	}
	return c
}

// withArrivalDefaults fills the thresholds that decide whether a car has
// arrived. Dwell is deliberately absent: zero is a LEGITIMATE value for it —
// tests use it to fire on the first qualifying frame — exactly as
// arrival.Config leaves the same field alone for the same reason.
func (c Config) withArrivalDefaults() Config {
	d := DefaultConfig()
	if c.ArrivalRadiusMeters <= 0 {
		c.ArrivalRadiusMeters = d.ArrivalRadiusMeters
	}
	if c.StoppedSpeedMPH <= 0 {
		c.StoppedSpeedMPH = d.StoppedSpeedMPH
	}
	if c.StillnessMeters <= 0 {
		c.StillnessMeters = d.StillnessMeters
	}
	if c.MaxStillnessGap <= 0 {
		c.MaxStillnessGap = d.MaxStillnessGap
	}
	return c
}
