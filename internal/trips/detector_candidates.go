package trips

import (
	"context"
	"log/slog"
	"time"
)

// The OPEN-WINDOW CANDIDATE CACHE. Split from detector.go so both stay inside
// the 300-line cap, and the seam is a real one: detector.go decides what a
// FRAME means, and this file decides which cars the detector is watching at
// all. The two change for different reasons — one on a rule about arrival, the
// other on a rule about staleness — and neither has to be read to understand
// the other.

// legCandidates serves the open-window vehicle set to the frame path, refreshed
// no more often than the TTL.
//
// LAZY, on the frame path, rather than on a background ticker — the same shape
// internal/arrival's candidate cache takes and for the same two reasons: a
// fleet where nothing is streaming costs zero queries, and the snapshot is only
// ever as old as the TTL at the moment it is actually used.
type legCandidates struct {
	store  TripStore
	cfg    Config
	logger *slog.Logger

	byVehicle map[string]TripVehicle
	// fetchedAt is when the SNAPSHOT was taken, and it is what the staleness
	// ceiling is measured against.
	fetchedAt time.Time
	// attemptedAt is when a refresh was last TRIED, successfully or not, and
	// it is what the TTL gate consults.
	//
	// The two were one field, and a failed refresh left it untouched: the gate
	// then read a snapshot that was already older than the TTL, so EVERY
	// SUBSEQUENT FRAME re-ran the query. A database blip therefore converted
	// the 15-second cache into a per-frame five-second-timeout read on the
	// single bus goroutine — up to one per second per streaming car, each
	// able to block the delivery loop for the whole timeout, which drops
	// frames for every other subscriber on the bus. Separating them keeps the
	// retry rate at one per TTL while leaving the ceiling honest about how old
	// the data actually is.
	attemptedAt time.Time
	failing     bool
}

func newLegCandidates(store TripStore, cfg Config, logger *slog.Logger) *legCandidates {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &legCandidates{store: store, cfg: cfg, logger: logger}
}

// ensure returns the vehicle-keyed snapshot, refreshing it when it has aged
// past the TTL. The second return reports whether THIS call installed a fresh
// one.
//
// WHEN THE DATABASE IS UNREACHABLE it serves the previous snapshot for up to
// staleSnapshotFactor TTLs and then an EMPTY one. Failing towards "detect
// nothing" is the only correct direction: a stale candidate set cannot be
// checked against a trip that ended, and opening a leg on a closed window would
// push a card to people whose access has already been revoked.
func (c *legCandidates) ensure(ctx context.Context, now time.Time) (map[string]TripVehicle, bool) {
	// Gated on the last ATTEMPT so a failing refresh backs off at the TTL
	// rather than re-firing on every frame, and so the "no windows anywhere"
	// answer is cached like any other.
	if !c.attemptedAt.IsZero() && now.Sub(c.attemptedAt) < c.cfg.CandidateTTL {
		return c.byVehicle, false
	}

	readCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	rows, err := c.store.ActiveTripVehicles(readCtx, c.cfg.CandidateLimit)
	c.attemptedAt = now
	if err != nil {
		return c.serveStale(now, err), false
	}

	if c.failing {
		c.logger.Info("trips: candidate refresh recovered",
			slog.Duration("outage", now.Sub(c.fetchedAt)))
		c.failing = false
	}
	next := make(map[string]TripVehicle, len(rows))
	for _, row := range rows {
		next[row.VehicleID] = row
	}
	if len(next) != len(c.byVehicle) {
		c.logger.Info("trips: open-window vehicle set rebuilt", slog.Int("vehicles", len(next)))
	}
	c.byVehicle, c.fetchedAt = next, now
	return c.byVehicle, true
}

// serveStale decides what to hand back after a failed refresh.
func (c *legCandidates) serveStale(now time.Time, err error) map[string]TripVehicle {
	age := now.Sub(c.fetchedAt)
	ceiling := time.Duration(staleSnapshotFactor) * c.cfg.CandidateTTL
	if c.byVehicle != nil && age <= ceiling {
		c.logger.Debug("trips: candidate refresh failed, serving cached set",
			slog.Duration("snapshot_age", age), slog.String("error", err.Error()))
		c.failing = true
		return c.byVehicle
	}
	if c.byVehicle != nil || !c.failing {
		c.logger.Warn("trips: open-window set unavailable, leg detection suspended",
			slog.Duration("snapshot_age", age), slog.String("error", err.Error()))
	}
	c.failing = true
	c.byVehicle = nil
	return map[string]TripVehicle{}
}

// staleSnapshotFactor is how many TTLs a snapshot may keep being served while
// refreshes fail. Four (a minute at the default TTL) rides out a failover and
// is short enough that legs are not decided from a picture nobody can confirm.
const staleSnapshotFactor = 4
