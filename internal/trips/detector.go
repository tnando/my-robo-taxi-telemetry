package trips

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

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

	// mu guards the lifecycle fields below. handleFrame reads ctx on every
	// frame while Start and Stop write all four, and although the bus almost
	// certainly serialises delivery against Unsubscribe, "almost certainly" is
	// not a synchronisation primitive — an implicit invariant on the busiest
	// path in the service is exactly the kind that a future bus rewrite
	// falsifies silently. The mutex is uncontended in steady state (one
	// uncontended Lock per frame) and makes the invariant something the race
	// detector can see.
	mu     sync.Mutex
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
	// The context is seeded ALREADY CANCELLED rather than left nil. A frame
	// delivered before Start — or after Stop, in the window before the bus
	// stops calling — would otherwise derive a timeout from a nil parent and
	// panic on the hot path; cancelled, every store call it makes fails fast
	// and the frame is dropped, which is what a detector that is not running
	// should do.
	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	return &Detector{
		svc:      svc,
		bus:      bus,
		vins:     vins,
		logger:   logger,
		cfg:      svc.cfg,
		trips:    newLegCandidates(svc.trips, svc.cfg, logger),
		vehicles: make(map[string]*vehicleState),
		ctx:      dead,
	}
}

// frameCtx returns the context the per-frame path runs under.
func (d *Detector) frameCtx() context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ctx
}

// Start subscribes to TopicVehicleTelemetry. The context governs the candidate
// reads and leg writes the handler makes, so cancelling it stops the detector
// touching the database even before Stop unsubscribes it.
func (d *Detector) Start(ctx context.Context) error {
	d.mu.Lock()
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.mu.Unlock()

	// Subscribed OUTSIDE the lock: the bus may deliver the first frame from
	// another goroutine before Subscribe returns, and that frame's frameCtx()
	// would deadlock against a lock this call still held.
	sub, err := d.bus.Subscribe(events.TopicVehicleTelemetry, d.handleFrame)
	if err != nil {
		_ = d.Stop()
		return fmt.Errorf("trips.Detector.Start: %w", err)
	}
	d.mu.Lock()
	d.sub, d.subbed = sub, true
	d.mu.Unlock()

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
	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
	}
	sub, subbed := d.sub, d.subbed
	d.subbed = false
	d.mu.Unlock()

	if !subbed {
		return nil
	}
	if err := d.bus.Unsubscribe(sub); err != nil {
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
	ctx := d.frameCtx()

	byVehicle, fresh := d.trips.ensure(ctx, d.svc.now())
	if fresh {
		// THE PRUNE IS STRUCTURAL, not a special case for the empty set.
		//
		// It used to clear the whole map only when NO window was open
		// anywhere, and drop one car's entry only on a frame that car itself
		// sent. A rolling fleet — cars entering and leaving windows with no
		// completely-empty moment between them — therefore grew the map
		// monotonically, and a car that left its window and then stopped
		// streaming kept its entry for the life of the process. Rebuilding it
		// against the fresh snapshot bounds it by the number of cars in an
		// open window, which is what it was always meant to be, and it also
		// makes the "a later trip begins from a clean slate" rule true for a
		// car that never sends another frame in between.
		d.pruneVehicles(byVehicle)
	}
	if len(byVehicle) == 0 {
		return
	}

	vehicleID, err := d.vins.ResolveID(ctx, te.VIN)
	if err != nil || vehicleID == "" {
		// A VIN with no vehicle row: a car mid-teardown, or one streaming
		// before its provisioning finished. Nothing to attribute a leg to.
		return
	}
	tv, watching := byVehicle[vehicleID]
	if !watching {
		// The overwhelmingly common case: a car in no open window. Its
		// memory is dropped so a trip starting later begins from a clean
		// slate rather than from a stale destination.
		delete(d.vehicles, vehicleID)
		return
	}

	state := d.vehicles[vehicleID]
	if state == nil {
		state = &vehicleState{}
		d.vehicles[vehicleID] = state
	}

	f := fixFrom(te)
	drivingBefore, destBefore := state.driving, state.destination
	state.apply(f, d.cfg)
	d.decide(ctx, tv, state, f, drivingBefore, destBefore)
}

// pruneVehicles drops the per-car memory of every vehicle that is not in the
// snapshot. Keyed by VEHICLE ID rather than by VIN precisely so this is
// possible: the candidate snapshot is vehicle-keyed, and a VIN cannot be
// compared against it without a resolver call the pruning path has no reason
// to make.
func (d *Detector) pruneVehicles(byVehicle map[string]TripVehicle) {
	for vehicleID := range d.vehicles {
		if _, watching := byVehicle[vehicleID]; !watching {
			delete(d.vehicles, vehicleID)
		}
	}
}
