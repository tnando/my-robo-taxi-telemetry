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

	// LegBannerWindow is how long a leg banner about ONE trip going to ONE
	// place silences its own repeat (MYR-620).
	//
	// ⚠ IT IS NOT A CLAIM ON A LEG, and that is the whole point. Every one of
	// the ten "Tesla is on the move — Heading to Element by Marriott Sedona."
	// banners on the client's 2026-09-08 lock screen was a correctly-claimed,
	// once-per-leg push; the LEG flapped, and `started_notified_at` is a stamp
	// on a ROW that each reopen replaced. The debounce and the resume make that
	// flap far rarer, and this makes the banner bounded whatever the detector
	// does — which is the property the person holding the phone cares about.
	LegBannerWindow time.Duration

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
