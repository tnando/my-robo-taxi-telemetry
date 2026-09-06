package trips

import (
	"context"
	"log/slog"
	"time"
)

// The window sweeper (MYR-602).
//
// Every 60 seconds it asks the database two questions — which trips have opened
// and which have closed — and each is answered by ONE claim statement that
// stamps and returns in the same breath. There is no read-then-write anywhere
// in this file, and that is the whole concurrency story: two processes during a
// rolling deploy both run this pass, and the stamps arbitrate.
//
// IT DOES NOT SWEEP IMMEDIATELY ON START, matching the AccessRevalidator's own
// reasoning: at startup there is nothing connected to re-mask, and a pass racing
// the first handshakes buys nothing. A trip that opened while the process was
// down is claimed on the first tick a minute later, which is inside the same
// latency budget every other edge has.

// Sweeper drives the window transitions on a ticker.
type Sweeper struct {
	svc    *Service
	logger *slog.Logger
}

// NewSweeper builds the sweeper over a Service.
func NewSweeper(svc *Service, logger *slog.Logger) *Sweeper {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Sweeper{svc: svc, logger: logger}
}

// Run sweeps every interval until ctx is cancelled. Intended to be started as a
// goroutine at wiring time.
func (s *Sweeper) Run(ctx context.Context) {
	if s.svc == nil {
		return
	}
	interval := s.svc.cfg.SweepInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.logger.Info("trip sweeper started",
		slog.Duration("interval", interval),
		slog.Int("limit", s.svc.cfg.SweepLimit),
	)

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("trip sweeper stopped")
			return
		case <-ticker.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepOnce runs a single pass and reports how many trips it opened and closed.
// Exported so a test can drive one deterministic pass instead of waiting on the
// ticker, and so an operator command can force one.
//
// ENDINGS ARE CLAIMED FIRST. A trip whose whole window elapsed while the
// process was down is BOTH unopened and closed, and the two claims are
// independent — so sweeping starts first would announce "your trip has started"
// and then, a few milliseconds later in the same pass, "your trip has ended".
// Claiming the ending first does not stop the start claim from matching (the
// start's own window predicate excludes a trip that is no longer open, and a
// trip ended early is excluded by the same LEAST expression), so the elapsed
// trip is settled silently and the participants hear once, about the thing that
// is actually true.
func (s *Sweeper) SweepOnce(ctx context.Context) (started, ended int) {
	// ONE RE-MASK SWEEP FOR THE WHOLE PASS. Every edge below asks for one, and
	// the ask is global — Revalidator.SweepOnce walks every connected session
	// and takes no argument — so N edges want exactly the work one sweep does.
	// Batching them here makes that a statement rather than a race with how
	// fast each edge happens to finish, and it is the difference between two
	// fleet-wide passes and four hundred after an outage. The release comes
	// AFTER both loops, so the single sweep observes every window that moved.
	s.svc.nudges.hold()
	defer s.svc.nudges.release(ctx)

	ended = s.sweepEndings(ctx)
	started = s.sweepStarts(ctx)

	if started > 0 || ended > 0 {
		s.logger.Info("trip windows swept",
			slog.Int("started", started),
			slog.Int("ended", ended),
		)
	}
	return started, ended
}

// sweepEndings claims and settles the trips whose effective end has passed.
func (s *Sweeper) sweepEndings(ctx context.Context) int {
	readCtx, cancel := context.WithTimeout(ctx, s.svc.cfg.Timeout)
	ids, err := s.svc.trips.ClaimTripsToEnd(readCtx, s.svc.cfg.SweepLimit)
	cancel()
	if err != nil {
		// FAIL QUIET AND TRY AGAIN NEXT MINUTE. A database blip must not take
		// the pass down or wedge the ticker; the claim is idempotent, so a
		// skipped pass costs one interval of staleness and nothing else. This
		// is the same posture the AccessRevalidator takes for the same reason.
		s.logger.Warn("trip sweep: end claim failed", slog.String("error", err.Error()))
		return 0
	}
	for _, id := range ids {
		// The stamp is ALREADY CLAIMED by the statement above, so this calls
		// the post-claim half directly rather than SettleTrip, which would
		// claim again and find nothing.
		s.perTrip(ctx, s.svc.settleClaimed, id)
	}
	return len(ids)
}

// sweepStarts claims and announces the trips whose window has opened.
func (s *Sweeper) sweepStarts(ctx context.Context) int {
	readCtx, cancel := context.WithTimeout(ctx, s.svc.cfg.Timeout)
	ids, err := s.svc.trips.ClaimTripsToStart(readCtx, s.svc.cfg.SweepLimit)
	cancel()
	if err != nil {
		s.logger.Warn("trip sweep: start claim failed", slog.String("error", err.Error()))
		return 0
	}
	for _, id := range ids {
		s.perTrip(ctx, s.svc.announceStart, id)
	}
	return len(ids)
}

// perTrip runs one claimed trip's post-claim work under its own deadline.
//
// EACH ID GETS ITS OWN BUDGET, and the reason is the batch. A pass may claim
// up to SweepLimit (200) endings and as many starts, and settleClaimed and
// announceStart each make several store calls plus a fan-out. Run on the root
// context they inherit no deadline at all, so ONE trip whose audience read is
// stuck on a struggling database holds up every remaining trip in that tick —
// and the trips behind it are the ones whose windows have already elapsed, so
// their participants keep live location for as long as the stall lasts. That
// is the one failure direction this feature must not have.
//
// The bound is EdgeTimeout rather than Timeout, because what is bounded here
// is a SEQUENCE — an audience read, the open legs' endings, the card pushes
// and the banner fan-out — of which the pushes are network calls to Apple. A
// single statement's five seconds would abandon a perfectly healthy edge
// serving a large audience; twenty is three times that and still a third of
// the interval, so the pass always finishes before the next one begins.
//
// The claim itself is NOT rolled back on a timeout, and that is correct: the
// stamp records that this edge was announced ONCE, and re-announcing a trip's
// ending on the next tick because the fan-out was slow is the repeating
// notification queryClaimTripsToEnd exists to make impossible.
func (s *Sweeper) perTrip(ctx context.Context, work func(context.Context, string), tripID string) {
	workCtx, cancel := context.WithTimeout(ctx, s.svc.cfg.EdgeTimeout)
	defer cancel()
	work(workCtx, tripID)
}
