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

	// Timeout bounds every store call the sweeper or the detector makes, so
	// neither a stalled claim nor a stalled candidate read can wedge the frame
	// path or hold a pass past its own interval.
	Timeout time.Duration

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

	defaultTimeout = 5 * time.Second

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
		ArrivalRadiusMeters: defaultArrivalRadiusMeters,
		Dwell:               defaultDwell,
		StoppedSpeedMPH:     defaultStoppedSpeedMPH,
		StillnessMeters:     defaultStillnessMeters,
		MaxStillnessGap:     defaultMaxStillnessGap,
		Timeout:             defaultTimeout,
		EdgeTimeout:         defaultEdgeTimeout,
		RevalidateTimeout:   defaultRevalidateTimeout,
	}
}

// withDefaults replaces zero-valued knobs with their defaults.
//
// Dwell is the one field where zero is a LEGITIMATE value — tests use it to
// fire on the first qualifying frame — so it is left alone, exactly as
// arrival.Config does for the same field and for the same reason. Every other
// zero would be a misconfiguration rather than a choice.
func (c Config) withDefaults() Config {
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
	if c.EdgeTimeout <= 0 {
		c.EdgeTimeout = d.EdgeTimeout
	}
	if c.RevalidateTimeout <= 0 {
		c.RevalidateTimeout = d.RevalidateTimeout
	}
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	return c
}
