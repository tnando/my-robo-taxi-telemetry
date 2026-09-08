package trips

import "time"

// Config tunes the sweeper and the leg detector. Zero fields take the
// production defaults via withDefaults, so a caller that only wants to override
// one knob — or a test that only cares about the dwell — cannot accidentally
// configure a zero radius.
type Config struct {
	// Enabled is the TRIPS_ENABLED kill-switch. It is read at COMPOSITION time,
	// not per pass: cmd/ simply does not construct the sweeper or the detector
	// when it is false, so a disabled deployment costs nothing and holds no
	// state.
	Enabled bool

	// SweepInterval is how often the window edges are checked. It is the whole
	// latency budget for a trip starting and ending — see the package doc — and
	// it deliberately MATCHES ws.DefaultRevalidateInterval, because a window
	// edge that is announced by a push but not yet reflected in the socket's
	// mask is a participant reading "your trip started" over a map that is
	// still blank.
	SweepInterval time.Duration
	// SweepLimit caps one pass's claim in each direction. A guard against a
	// pathological scan, not a rationing policy: a fleet cannot plausibly cross
	// 200 window edges in one minute, and if it did the next pass takes the
	// rest one minute later.
	SweepLimit int

	// CandidateTTL is how long the open-window vehicle set is served before it
	// is re-read. It bounds how late a trip that just opened can be noticed by
	// the leg detector — irrelevant in practice, since a car has to start
	// driving afterwards — and how long a window that just CLOSED can still
	// open a leg, which is the direction worth keeping short.
	CandidateTTL time.Duration
	// CandidateLimit caps one candidate refresh.
	CandidateLimit int

	// ArrivalRadiusMeters, Dwell, StoppedSpeedMPH, StillnessMeters and
	// MaxStillnessGap are internal/arrival's thresholds, reused VERBATIM rather
	// than re-derived. The two detectors are answering the same physical
	// question — "is this car stopped at that place?" — and two sets of numbers
	// for one question is how they end up disagreeing about the same car in the
	// same car park. See arrival.DefaultConfig for the derivation of each.
	ArrivalRadiusMeters float64
	Dwell               time.Duration
	StoppedSpeedMPH     float64
	StillnessMeters     float64
	MaxStillnessGap     time.Duration

	// LegClearGrace is how long a car may report an EMPTY destination name,
	// while still driving and still reporting an arrival estimate, before the
	// detector believes the route was actually cancelled (MYR-612).
	//
	// Zero is not a legitimate value here and withDefaults replaces it: a zero
	// grace is the pre-MYR-612 behaviour, which closed a leg on one delta that
	// happened to carry an empty name and re-opened it two seconds later.
	LegClearGrace time.Duration

	// LegMergeWindow is how soon after a leg closed WITHOUT ARRIVING the same
	// car may resume it, rather than start a second one, when it sets off again
	// for the same place.
	//
	// It is the second line of defence behind LegClearGrace, for the closes
	// that debouncing cannot prevent — a process restart between the two
	// frames, two servers during a rolling deploy, a grace that expired one
	// frame before the name came back. What it buys is that the journey stays
	// ONE leg: one `trip_leg_started` banner, one card, one row in the trip's
	// history.
	LegMergeWindow time.Duration

	// LegReadTTL is how long the detector serves a car's open-leg answer from
	// memory before re-reading it (MYR-612).
	//
	// It exists because that read was the last per-frame statement on the bus
	// goroutine, and the fact it reads changes at most twice per journey. The
	// cost of the cache is a few seconds of latency on a leg edge, against a
	// twenty-second arrival dwell and a twenty-second card floor; the cost of
	// NOT having it was a sustained query per second per car in an open window,
	// which is how one road trip starved the pool an unrelated JWT existence
	// probe then timed out against.
	LegReadTTL time.Duration

	// Timeout bounds every store call the SWEEPER makes, so a stalled claim
	// cannot hold a pass past its own interval.
	//
	// The frame path does not use it — see FrameTimeout.
	Timeout time.Duration

	// FrameTimeout bounds ONE FRAME's whole journey through the detector, and
	// it is the only deadline the per-frame path has (MYR-612 review).
	//
	// ⚠ ONE PLACE, AT THE CHOKEPOINT, because the property that matters is
	// about the BUS GOROUTINE and not about any one statement: every frame is
	// handled on the single goroutine the event bus delivers on, so anything
	// that blocks in here blocks every other subscriber of every other car.
	// The budget used to be applied at each store call instead — six of them —
	// and the seventh, the VIN→vehicle resolution, had no budget at all: on a
	// cache miss it was an unbounded query on that goroutine, which is exactly
	// the shape the architecture doc claims cannot exist. A wrapper per call
	// site is a rule the next call site can forget; a wrapper at the entrance
	// is a rule it cannot.
	//
	// Sized for the most expensive legitimate frame — a candidate refresh, a
	// VIN resolution, an open-leg read AND a leg edge with its APNs pushes —
	// because a frame carrying an edge is the one frame that must not be cut
	// short. It is a CEILING on damage, not a target: an ordinary frame
	// touches nothing at all.
	FrameTimeout time.Duration

	// EdgeTimeout bounds ONE claimed trip's post-claim work in a sweeper pass
	// — its audience read, its open legs' endings, its card pushes and its
	// banner fan-out taken together. Larger than Timeout because it covers a
	// SEQUENCE that includes network sends to APNs, and far smaller than
	// SweepInterval because a pass must finish before the next one starts.
	EdgeTimeout time.Duration

	// RevalidateTimeout bounds ONE coalesced re-mask sweep (see
	// revalidate.go). Its own constant rather than a reuse of Timeout because
	// the two bound different things: Timeout bounds a single indexed
	// statement, while a sweep walks every connected session and resolves a
	// role per (session, vehicle) — a fleet-wide pass that legitimately takes
	// longer than any one query, and that must still not be able to hold the
	// single-flight latch shut across a whole sweeper tick.
	RevalidateTimeout time.Duration
}

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
