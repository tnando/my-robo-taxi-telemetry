package trips

import (
	"context"
	"log/slog"
	"sync"
)

// The re-mask nudge's single-flight (MYR-602 review).
//
// Every trip transition wants the same thing from the WebSocket layer: re-derive
// the roles now, because a window edge is invisible to every other mechanism in
// the service. But `Revalidator.SweepOnce` is GLOBAL — it walks every connected
// session on the hub — so the nudge is not per-trip work, and running one per
// trip is running the same pass over and over.
//
// That distinction is what makes coalescing SAFE rather than merely cheap: the
// sweep takes no argument and carries no per-trip state, so a single pass that
// starts after the last edge was recorded serves every edge that asked for one.
// The only property a caller needs is "a sweep will run, and it will observe
// what I just wrote", and the trailing pass below is exactly that promise.
//
// WHAT IT IS NOT. It is not a debounce with a timer: there is no delay, and the
// first request runs immediately. It is not a queue: the requests carry no
// payload to queue. It is a latch — at most one sweep in flight, at most one
// pending behind it.

// revalidationNudge runs at most one re-mask sweep at a time, with at most one
// trailing pass behind it.
type revalidationNudge struct {
	svc *Service

	mu       sync.Mutex
	inFlight bool
	// pending records that a request arrived while a sweep was running, so the
	// runner takes one more pass before stopping. It is what makes the promise
	// "a sweep will observe what you just wrote" hold: the write happened
	// before the request, and the trailing pass starts after it.
	pending bool
	// reason and tripID are the LAST requester's, kept only for the log line.
	// A coalesced pass genuinely serves several edges, and naming one of them
	// is more useful to an operator than naming none.
	reason string
	tripID string

	// held counts open BATCHES. While one is open every request only records
	// that a sweep is wanted; the release runs exactly one.
	//
	// It is what turns "at most two sweeps per pass" from a race into a
	// statement. A sweeper tick may claim up to SweepLimit edges in each
	// direction and each of them asks for a nudge, but a nudge is only ever
	// wanted ONCE per pass — the sweep is global and takes no argument — and
	// without the batch the count depends on how fast each pass happens to
	// finish relative to the next request. See Sweeper.SweepOnce.
	held int

	// wg tracks the runner so shutdown and tests can wait for it.
	wg sync.WaitGroup
}

func newRevalidationNudge(svc *Service) *revalidationNudge {
	return &revalidationNudge{svc: svc}
}

// request asks for a sweep. Returns immediately, always.
func (n *revalidationNudge) request(ctx context.Context, reason, tripID string) {
	n.mu.Lock()
	n.reason, n.tripID = reason, tripID
	if n.inFlight || n.held > 0 {
		n.pending = true
		n.mu.Unlock()
		return
	}
	n.inFlight = true
	n.wg.Add(1)
	n.mu.Unlock()

	go n.run(ctx)
}

// hold opens a batch. Every request until the matching release only records
// that a sweep is wanted.
func (n *revalidationNudge) hold() {
	n.mu.Lock()
	n.held++
	n.mu.Unlock()
}

// release closes a batch and runs the one sweep it accumulated, if any.
func (n *revalidationNudge) release(ctx context.Context) {
	n.mu.Lock()
	if n.held > 0 {
		n.held--
	}
	if n.held > 0 || !n.pending || n.inFlight {
		// Still batched, nothing asked, or a runner is already going to take
		// the pending flag on its next lap.
		n.mu.Unlock()
		return
	}
	n.pending = false
	n.inFlight = true
	n.wg.Add(1)
	n.mu.Unlock()

	go n.run(context.WithoutCancel(ctx))
}

// run sweeps until nothing further has been requested.
func (n *revalidationNudge) run(ctx context.Context) {
	defer n.wg.Done()
	for {
		n.mu.Lock()
		reason, tripID := n.reason, n.tripID
		n.mu.Unlock()

		n.sweep(ctx, reason, tripID)

		n.mu.Lock()
		if !n.pending || n.held > 0 {
			// Nothing further asked, or a batch has opened since — in which
			// case the release owns the trailing pass and starting one here
			// would be the per-edge sweep all over again.
			n.inFlight = false
			n.mu.Unlock()
			return
		}
		n.pending = false
		n.mu.Unlock()
	}
}

// sweep runs one pass under its own timeout.
//
// The bound is the sweeper's own tripSweepTimeout-shaped Config.Timeout rather
// than the caller's remaining budget: the caller is frequently an HTTP handler
// that has already answered, so there is no budget left to inherit, and an
// unbounded sweep on a struggling hub would hold the latch shut and starve
// every later edge of the pass it asked for.
func (n *revalidationNudge) sweep(ctx context.Context, reason, tripID string) {
	if n.svc == nil || n.svc.revalidate == nil {
		return
	}
	sweepCtx, cancel := context.WithTimeout(ctx, n.svc.cfg.RevalidateTimeout)
	defer cancel()

	closed := n.svc.revalidate.SweepOnce(sweepCtx)
	n.svc.logger.Debug("trips: nudged access revalidation",
		slog.String("reason", reason),
		slog.String("trip_id", tripID),
		slog.Int("sessions_closed", closed),
	)
}

// drain waits for the runner to finish, including any trailing pass.
func (n *revalidationNudge) drain() {
	if n == nil {
		return
	}
	n.wg.Wait()
}
