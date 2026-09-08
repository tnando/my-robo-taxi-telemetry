// The sweep itself: list, decide, push, tally. The narrative is in doc.go and
// the vocabulary in types.go.

package fleetrepush

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Sweep re-pushes the current DefaultFieldConfig to already-streaming cars.
type Sweep struct {
	deps   Deps
	cfg    Config
	logger *slog.Logger
	// audited remembers which owners already have their MYR-447 decrypt row for
	// this run. One owner with three cars is one decrypt of one credential
	// pair, and writing three rows for it would overstate what happened.
	audited map[string]bool
	// teslaCalls counts round-trips made, so the rate limiter waits BETWEEN
	// calls rather than before the first one.
	teslaCalls int
}

// New builds a sweep. A nil logger discards.
func New(deps Deps, cfg Config, logger *slog.Logger) *Sweep {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Sweep{deps: deps, cfg: cfg, logger: logger, audited: map[string]bool{}}
}

// Run performs one pass and returns the report.
//
// A returned error still comes with the report built so far: a run cancelled
// halfway through, or aborted by an audit-write failure, has already changed
// real cars and the operator must be able to see which.
func (s *Sweep) Run(ctx context.Context) (Report, error) {
	limit := s.cfg.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	mode := "dry-run"
	if s.cfg.Apply {
		mode = "apply"
	}
	rep := Report{
		Mode:           mode,
		Limit:          limit,
		SkipReasons:    map[string]int{},
		FailureReasons: map[string]int{},
		Vehicles:       []VehicleReport{},
	}

	rows, err := s.deps.Store.ListStreamingFleetConfigVehicles(ctx, limit)
	if err != nil {
		return rep, fmt.Errorf("list streaming vehicles: %w", err)
	}
	s.logger.Info("fleet-config re-push sweep starting",
		slog.String("mode", mode),
		slog.Int("candidates", len(rows)),
		slog.Int("limit", limit))

	for i := range rows {
		line, fatal := s.sweepOne(ctx, rows[i])
		if fatal != nil {
			return rep, fatal
		}
		rep.add(line)
	}
	s.logger.Info("fleet-config re-push sweep finished",
		slog.String("mode", mode),
		slog.Int("pushed", rep.Pushed),
		slog.Int("would_push", rep.WouldPush),
		slog.Int("skipped", rep.Skipped),
		slog.Int("failed", rep.Failed))
	return rep, nil
}

// sweepOne decides and, under --apply, acts on one car. The second return is
// FATAL to the whole run — reserved for a failed audit write and a cancelled
// context, neither of which the next vehicle can be attempted after.
//
// ORDER IS THE POINT. The three row-shaped refusals come first because they
// cost nothing: no token is decrypted, no audit row is written and no Tesla
// call is made for a car the sweep was never going to touch. Only then does the
// owner's credential come out of the database.
func (s *Sweep) sweepOne(ctx context.Context, c Candidate) (VehicleReport, error) {
	line := VehicleReport{
		VIN:         c.VIN,
		UserID:      c.UserID,
		VehicleName: c.VehicleName,
		LastUpdated: c.LastUpdated,
	}
	if reason, refused := rowRefusal(c); refused {
		return s.skip(line, reason), nil
	}

	if err := s.audit(ctx, c.UserID); err != nil {
		return line, err
	}

	token, err := s.deps.Tokens.AccessToken(ctx, c.UserID)
	switch {
	case errors.Is(err, ErrNoToken):
		// Permanent, not transient: no Tesla row on file, so nothing can
		// authenticate a push for this car ever.
		return s.skip(line, ReasonNoToken), nil
	case err != nil:
		// An expired-but-unrefreshable token lands here rather than in
		// no_token, on the fleetorphan reading: a refresh failure can be
		// Tesla-side and is worth retrying.
		return s.fail(line, ReasonTokenFailed, err), nil
	}

	if err := s.pace(ctx); err != nil {
		return line, err
	}
	status, err := s.deps.Reader.GetTelemetryConfig(ctx, token, c.VIN)
	switch {
	case errors.Is(err, ErrNoConfig):
		return s.skip(line, ReasonNoConfig), nil
	case err != nil:
		return s.fail(line, ReasonConfigReadFailed, err), nil
	case !status.Configured:
		// Streaming is what this sweep re-pushes. A car with no config is the
		// reconciler's candidate, not this tool's — pushing here would heal it
		// as a side effect, outside the schedule and backoff that exist to
		// keep those attempts bounded.
		return s.skip(line, ReasonNoConfig), nil
	}
	line.ConfigAgeDays = s.configAge(status.Exp)

	if !s.cfg.Apply {
		line.Action = ActionWouldPush
		s.logger.Info("fleet-config re-push: would push",
			slog.String("vin", redactVIN(c.VIN)),
			slog.String("user_id", c.UserID),
			slog.Any("config_age_days", line.ConfigAgeDays))
		return line, nil
	}

	if err := s.pace(ctx); err != nil {
		return line, err
	}
	if err := s.deps.Pusher.PushForVIN(ctx, token, c.VIN); err != nil {
		if s.deps.Classify != nil && s.deps.Classify.IsAwaitingVirtualKey(err) {
			// Tesla answered 200 and did nothing: the virtual key is not
			// enrolled. Not a fault of the sweep and not a retryable error —
			// a skip with the reason named.
			return s.skip(line, ReasonMissingKey), nil
		}
		return s.fail(line, ReasonPushFailed, err), nil
	}
	line.Action = ActionPushed
	s.logger.Info("fleet-config re-push: config pushed",
		slog.String("event", "fleet_config_repushed"),
		slog.String("vin", redactVIN(c.VIN)),
		slog.String("user_id", c.UserID))
	return line, nil
}

// rowRefusal answers the three refusals that are decidable from the candidate
// row alone, in the order they cost nothing to check.
//
// Kept out of sweepOne so the sweep reads as "refuse, authorize, read, push"
// rather than as a wall of guards — the parseFleetConfigPushInput precedent.
func rowRefusal(c Candidate) (reason string, refused bool) {
	switch {
	case c.PendingOwnerAck:
		// Defence in depth: the candidate query already excludes these
		// (MYR-599 consent-wins). Enforced again at the consumer site so a
		// future producer inherits the gate instead of re-opening it.
		return ReasonAwaitingOwnerAck, true
	case c.Suspended:
		// MYR-592 removed this car's config on purpose, for cost. Re-pushing
		// would silently reverse that and start the bill again; the owner's
		// §7.28 reconnect is the only thing that may.
		return ReasonOwnerSuspended, true
	case c.ConfigAbsent:
		// The last push did not take. There is nothing to refresh and the
		// MYR-448 reconciler already owns the retry.
		return ReasonConfigAbsent, true
	}
	return "", false
}

// audit writes the owner's MYR-447 decrypt row once per run, before the token
// is read. A failure aborts the run: an unattributable decrypt is exactly what
// the audit exists to prevent, and continuing would produce more of them.
func (s *Sweep) audit(ctx context.Context, userID string) error {
	if s.deps.Auditor == nil || s.audited[userID] {
		return nil
	}
	if err := s.deps.Auditor.RecordTokenDecrypt(ctx, userID); err != nil {
		return fmt.Errorf("record operator decrypt(user=%s): %w", userID, err)
	}
	s.audited[userID] = true
	return nil
}

// pace enforces the rate limit between Tesla round-trips, and is the one place
// a cancelled context stops the run promptly.
func (s *Sweep) pace(ctx context.Context) error {
	interval := s.cfg.Interval
	if interval == 0 {
		interval = DefaultInterval
	}
	if s.teslaCalls > 0 && interval > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("rate limit wait: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sweep cancelled: %w", err)
	}
	s.teslaCalls++
	return nil
}

// configAge turns Tesla's echoed exp into the age of the config in days.
// Negative ages (a clock skew, or an exp we did not write) clamp to zero.
func (s *Sweep) configAge(exp *int64) *float64 {
	if exp == nil {
		return nil
	}
	age := configLifetime - time.Unix(*exp, 0).Sub(s.now())
	if age < 0 {
		age = 0
	}
	days := age.Hours() / 24
	return &days
}

func (s *Sweep) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

func (s *Sweep) skip(line VehicleReport, reason string) VehicleReport {
	line.Action = ActionSkipped
	line.Reason = reason
	s.logger.Info("fleet-config re-push: skipped",
		slog.String("vin", redactVIN(line.VIN)),
		slog.String("user_id", line.UserID),
		slog.String("reason", reason))
	return line
}

func (s *Sweep) fail(line VehicleReport, reason string, err error) VehicleReport {
	line.Action = ActionFailed
	line.Reason = reason
	line.Error = err.Error()
	s.logger.Warn("fleet-config re-push: failed",
		slog.String("vin", redactVIN(line.VIN)),
		slog.String("user_id", line.UserID),
		slog.String("reason", reason),
		slog.String("error", err.Error()))
	return line
}

// add folds one line into the tallies.
func (r *Report) add(line VehicleReport) {
	r.Examined++
	r.Vehicles = append(r.Vehicles, line)
	switch line.Action {
	case ActionPushed:
		r.Pushed++
	case ActionWouldPush:
		r.WouldPush++
	case ActionSkipped:
		r.Skipped++
		r.SkipReasons[line.Reason]++
	case ActionFailed:
		r.Failed++
		r.FailureReasons[line.Reason]++
	}
}

// redactVIN keeps the last 4 characters, per data-classification §2.1. The
// report on stdout carries whole VINs — an operator needs them — but the logs
// this writes alongside it must not.
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}

// discardWriter drops log output for the nil-logger default.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
