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
		s.svc.settleClaimed(ctx, id)
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
		s.svc.announceStart(ctx, id)
	}
	return len(ids)
}
