package trips

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// The LEG DETECTOR. See the package doc for why it reads raw telemetry rather
// than `drive.started`, and for the arrival-evidence inversion against
// internal/arrival.
//
// COST. It subscribes to the busiest topic in the service — up to one frame per
// second per streaming car — so the per-frame path is deliberately cheap: a VIN
// lookup in a cached map, and for the overwhelming majority of frames (cars in
// no open window at all) nothing else. Only one frame per CandidateTTL pays for
// a query, and only a genuine leg edge pays for a write.

// VINResolver maps a streaming VIN onto a vehicle cuid. Declared at the
// consumer site; satisfied by the same *store.VINCache the WS broadcaster uses,
// so the two cannot disagree about which car a VIN is.
type VINResolver interface {
	ResolveID(ctx context.Context, vin string) (string, error)
}

// Detector watches telemetry frames and opens and closes trip legs.
type Detector struct {
	svc      *Service
	bus      events.Bus
	vins     VINResolver
	logger   *slog.Logger
	cfg      Config
	trips    *legCandidates
	vehicles map[string]*vehicleState

	sub    events.Subscription
	subbed bool
	ctx    context.Context
	cancel context.CancelFunc
}

// NewDetector builds the detector over a Service.
func NewDetector(svc *Service, bus events.Bus, vins VINResolver, logger *slog.Logger) *Detector {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Detector{
		svc:      svc,
		bus:      bus,
		vins:     vins,
		logger:   logger,
		cfg:      svc.cfg,
		trips:    newLegCandidates(svc.trips, svc.cfg, logger),
		vehicles: make(map[string]*vehicleState),
	}
}

// Start subscribes to TopicVehicleTelemetry. The context governs the candidate
// reads and leg writes the handler makes, so cancelling it stops the detector
// touching the database even before Stop unsubscribes it.
func (d *Detector) Start(ctx context.Context) error {
	d.ctx, d.cancel = context.WithCancel(ctx)

	sub, err := d.bus.Subscribe(events.TopicVehicleTelemetry, d.handleFrame)
	if err != nil {
		d.cancel()
		return fmt.Errorf("trips.Detector.Start: %w", err)
	}
	d.sub, d.subbed = sub, true

	d.logger.Info("trip leg detector started",
		slog.Float64("arrival_radius_meters", d.cfg.ArrivalRadiusMeters),
		slog.Duration("dwell", d.cfg.Dwell),
		slog.Duration("candidate_ttl", d.cfg.CandidateTTL),
	)
	return nil
}

// Stop unsubscribes and cancels in-flight store work. Safe on a detector that
// never started.
func (d *Detector) Stop() error {
	if d.cancel != nil {
		d.cancel()
	}
	if !d.subbed {
		return nil
	}
	d.subbed = false
	if err := d.bus.Unsubscribe(d.sub); err != nil {
		return fmt.Errorf("trips.Detector.Stop: %w", err)
	}
	return nil
}

// handleFrame is the per-frame path.
func (d *Detector) handleFrame(evt events.Event) {
	te, ok := evt.Payload.(events.VehicleTelemetryEvent)
	if !ok || te.VIN == "" {
		return
	}

	byVehicle, fresh := d.trips.ensure(d.ctx, d.svc.now())
	if len(byVehicle) == 0 {
		if fresh {
			// No open windows anywhere. Drop the per-VIN memory rather than
			// carry it: the next window's first leg must be decided from live
			// frames, not from a destination this car reported an hour ago.
			d.vehicles = make(map[string]*vehicleState)
		}
		return
	}

	vehicleID, err := d.vins.ResolveID(d.ctx, te.VIN)
	if err != nil || vehicleID == "" {
		// A VIN with no vehicle row: a car mid-teardown, or one streaming
		// before its provisioning finished. Nothing to attribute a leg to.
		return
	}
	tv, watching := byVehicle[vehicleID]
	if !watching {
		// The overwhelmingly common case: a car in no open window. Its
		// per-VIN memory is dropped so a trip starting later begins from a
		// clean slate rather than from a stale destination.
		delete(d.vehicles, te.VIN)
		return
	}

	state := d.vehicles[te.VIN]
	if state == nil {
		state = &vehicleState{}
		d.vehicles[te.VIN] = state
	}

	f := fixFrom(te)
	drivingBefore, destBefore := state.driving, state.destination
	state.apply(f, d.cfg)
	d.decide(tv, state, f, drivingBefore, destBefore)
}

// decide acts on the edges one frame produced.
//
// THE OPEN CONDITION IS A TRANSITION INTO "driving, with a destination", from
// EITHER SIDE — the car started moving with a route already set, or it was
// already moving and a route appeared. The second case is the common one (a
// driver sets the destination on the dash after pulling out) and is exactly
// what a drive-start event could never have expressed; see the package doc.
//
// A leg that is already open is not re-opened: StartLeg is idempotent against
// the one-open-leg-per-trip index, and a car that RE-ROUTES mid-leg keeps its
// leg rather than getting a second card for one journey.
func (d *Detector) decide(tv TripVehicle, state *vehicleState, f fix, drivingBefore bool, destBefore string) {
	leg, err := d.svc.legs.OpenLegForVehicle(d.ctx, tv.VehicleID)
	if err != nil {
		d.logger.Warn("trips: open-leg lookup failed",
			slog.String("vehicle_id", tv.VehicleID), slog.String("error", err.Error()))
		return
	}

	underway := state.driving && state.destination != ""
	if !leg.Open() {
		if underway && (!drivingBefore || destBefore == "") {
			d.svc.openLeg(d.ctx, tv, state.destination, f.at)
		}
		return
	}

	// A leg IS open. Three things can close it, and they are checked in the
	// order of how much they assert.
	audience, audErr := d.svc.trips.TripAudienceFor(d.ctx, tv.TripID)
	if audErr != nil {
		d.logger.Warn("trips: leg audience lookup failed; deferring the leg edge",
			slog.String("leg_id", leg.ID), slog.String("error", audErr.Error()))
		return
	}

	// Computed BEFORE the dwell is folded in, because `arrivedAt` mutates the
	// track and this is a pure question about this frame alone.
	atDestination := state.inRadius(f, d.cfg)

	// 1. ARRIVAL — the strongest claim, and the only one that fires
	//    `trip_leg_arrived`. Latched so the twenty further qualifying frames
	//    that arrive while the car sits there do nothing.
	if !state.arrivalLatched && state.arrivedAt(f, d.cfg) {
		state.arrivalLatched = true
		d.svc.closeLeg(d.ctx, leg, audience, true)
		return
	}

	// 2. THE ROUTE WAS CLEARED. The driver cancelled navigation; the car may
	//    still be moving. The leg is over as a leg — there is no longer a place
	//    it is going — and it ended without evidence.
	if state.destination == "" {
		d.svc.closeLeg(d.ctx, leg, audience, false)
		return
	}

	// 3. THE CAR PARKED SHORT of its destination. `completed`, not `arrived`:
	//    it stopped somewhere, and nothing says it was the right somewhere.
	//
	//    THE `!atDestination` GUARD IS LOAD-BEARING AND WAS THE FIRST BUG THIS
	//    DETECTOR'S TESTS FOUND. A car that ARRIVES also stops, so without it
	//    every successful arrival was closed as `completed` by this branch on
	//    the very first stopped frame — one second into a dwell that needs
	//    twenty — and the arrival could never fire. A stop INSIDE the radius is
	//    the beginning of an arrival, not the end of a leg; the dwell decides
	//    which, and until it does the leg stays open.
	//
	//    The residual case is a car that parks at its destination and then goes
	//    completely silent, with not even a MYR-394 REST poll frame to satisfy
	//    the dwell. Its leg stays open until the window closes and is then
	//    settled as `completed`. That is the honest answer — nothing ever
	//    proved it stayed — and it is the same dependency internal/arrival has
	//    on the same poller for the same reason.
	if !state.driving && drivingBefore && !atDestination {
		d.svc.closeLeg(d.ctx, leg, audience, false)
		return
	}

	// Still under way. Refresh the card only when this frame's arrival estimate
	// has earned a push — see vehicleState.dueForCard.
	if minutes := etaMinutesFrom(f); state.dueForCard(minutes, f.at) {
		d.svc.updateLeg(d.ctx, leg, audience, f, minutes)
	}
}

// updateLeg refreshes a running card mid-leg.
//
// THE THROTTLE IS THE CALLER'S (vehicleState.dueForCard) rather than this
// function's, because it is per-VIN state and this is a stateless projection.
// What matters is that it exists: Apple throttles high-frequency Activity
// pushes by budget and a car streams up to once per second, so a refresh has to
// earn its push — the arrival minute must have moved, and a floor interval must
// have passed. The card's 3-minute stale-date is what keeps it honest between
// pushes.
//
// It lives on Service rather than Detector because the leg lifecycle belongs
// together, and because a future ticker could call it on the same terms.
func (s *Service) updateLeg(ctx context.Context, leg Leg, audience TripAudience, f fix, minutes *int) {
	if s.activities == nil || minutes == nil {
		return
	}
	tc := s.legContext(ctx, leg, audience, tripStatusEnroute, &f.at)
	tc.ETAMinutes = minutes
	s.activities.UpdateLeg(ctx, tc)
}

// etaMinutesFrom reads the car's own arrival estimate off a frame.
//
// NIL WHEN THE CAR DOES NOT SAY, never a computed guess. There is no route
// solver in this service, and MYR-194 rules out inventing a number in as many
// words: an absent `eta` renders a card with no time, which is a first-class
// state the client already handles.
func etaMinutesFrom(f fix) *int {
	if f.minutesToGo == nil {
		return nil
	}
	m := int(*f.minutesToGo + 0.5)
	if m < 0 {
		return nil
	}
	return &m
}

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
	fetchedAt time.Time
	failing   bool
}

func newLegCandidates(store TripStore, cfg Config, logger *slog.Logger) *legCandidates {
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
	if c.byVehicle != nil && now.Sub(c.fetchedAt) < c.cfg.CandidateTTL {
		return c.byVehicle, false
	}

	readCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	rows, err := c.store.ActiveTripVehicles(readCtx, c.cfg.CandidateLimit)
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
