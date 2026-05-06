package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/geocode"
)

// destGeocodeTimeout caps each per-flush reverse-geocode call so a slow
// or unreachable Mapbox endpoint cannot block the writer's flush loop.
// Set well under the writer's flush interval so a single laggy call
// does not stall successive ticks.
const destGeocodeTimeout = 5 * time.Second

// vehicleUpdater is the consumer-site interface for writing vehicle
// telemetry updates to the database.
type vehicleUpdater interface {
	UpdateTelemetry(ctx context.Context, vin string, update VehicleUpdate) error
	UpdateStatus(ctx context.Context, vin string, status VehicleStatus) error
}

// drivePersister is the consumer-site interface for persisting drive
// records to the database.
type drivePersister interface {
	Create(ctx context.Context, drive DriveRecord) error
	Complete(ctx context.Context, driveID string, stats DriveCompletion) error
	AppendRoutePoints(ctx context.Context, driveID string, points []RoutePointRecord) error
}

// WriterConfig holds tunable parameters for the Writer's batch flush behavior.
type WriterConfig struct {
	FlushInterval time.Duration
	BatchSize     int
	RouteBuffer   RouteBufferConfig
}

// DefaultWriterConfig returns production-ready defaults.
func DefaultWriterConfig() WriterConfig {
	return WriterConfig{
		FlushInterval: 5 * time.Second,
		BatchSize:     100,
		RouteBuffer:   DefaultRouteBufferConfig(),
	}
}

// Writer subscribes to telemetry and drive events on the event bus and
// persists them to the database. Telemetry updates are coalesced per VIN
// and flushed in batches on a timer or when the batch size is reached.
type Writer struct {
	vehicles vehicleUpdater
	drives   drivePersister
	bus      events.Bus
	vinCache *VINCache
	geocoder geocode.Geocoder
	logger   *slog.Logger
	cfg      WriterConfig
	routeBuf *routeBuffer

	pendingMu sync.Mutex
	pending   map[string]*VehicleUpdate // VIN → coalesced update
	count     int                       // total telemetry events since last flush

	// destAddrCache memoizes the most recent (lat, lng) → address
	// reverse-geocode result per VIN so a stable navigation destination
	// streamed every flush window does not burn the Mapbox rate budget
	// or cycle the same address through the DB on each interval. Cleared
	// for a VIN when the destination GPS is cleared (navigation cancelled
	// — atomic group clear per data-lifecycle.md §6.1).
	destAddrMu    sync.Mutex
	destAddrCache map[string]destAddrEntry

	subs      []events.Subscription
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	flushDone chan struct{} // closed when flushLoop goroutine exits
}

// NewWriter creates a Writer that will subscribe to telemetry and drive
// events, coalesce vehicle updates, and flush them periodically. The
// geocoder is used to reverse geocode drive start/end locations. Pass
// geocode.NoopGeocoder{} to disable geocoding.
func NewWriter(
	vehicles vehicleUpdater,
	drives drivePersister,
	vinLookup vinIDLookup,
	bus events.Bus,
	geocoder geocode.Geocoder,
	logger *slog.Logger,
	cfg WriterConfig,
) *Writer {
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultWriterConfig().FlushInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultWriterConfig().BatchSize
	}
	return &Writer{
		vehicles:      vehicles,
		drives:        drives,
		bus:           bus,
		vinCache:      NewVINCache(vinLookup, logger),
		geocoder:      geocoder,
		logger:        logger,
		cfg:           cfg,
		routeBuf:      newRouteBuffer(drives, logger, cfg.RouteBuffer),
		pending:       make(map[string]*VehicleUpdate),
		destAddrCache: make(map[string]destAddrEntry),
		done:          make(chan struct{}),
		flushDone:     make(chan struct{}),
	}
}

// destAddrEntry pairs a (lat, lng) with the address Mapbox returned for
// that pair. Used to short-circuit redundant geocoder calls when a
// vehicle's destination GPS is unchanged across flush windows.
type destAddrEntry struct {
	lat     float64
	lng     float64
	address string
}

// Start subscribes to telemetry and drive events and launches the flush
// ticker goroutine. It blocks until subscriptions are registered, then
// returns. Call Stop to shut down.
func (w *Writer) Start(ctx context.Context) error {
	telSub, err := w.bus.Subscribe(events.TopicVehicleTelemetry, w.handleTelemetry)
	if err != nil {
		return fmt.Errorf("Writer.Start: subscribe telemetry: %w", err)
	}

	startSub, err := w.bus.Subscribe(events.TopicDriveStarted, w.handleDriveStarted())
	if err != nil {
		_ = w.bus.Unsubscribe(telSub)
		return fmt.Errorf("Writer.Start: subscribe drive.started: %w", err)
	}

	updatedSub, err := w.bus.Subscribe(events.TopicDriveUpdated, w.handleDriveUpdated())
	if err != nil {
		_ = w.bus.Unsubscribe(telSub)
		_ = w.bus.Unsubscribe(startSub)
		return fmt.Errorf("Writer.Start: subscribe drive.updated: %w", err)
	}

	endSub, err := w.bus.Subscribe(events.TopicDriveEnded, w.handleDriveEnded())
	if err != nil {
		_ = w.bus.Unsubscribe(telSub)
		_ = w.bus.Unsubscribe(startSub)
		_ = w.bus.Unsubscribe(updatedSub)
		return fmt.Errorf("Writer.Start: subscribe drive.ended: %w", err)
	}

	w.subs = []events.Subscription{telSub, startSub, updatedSub, endSub}

	// #nosec G118 -- cancel is deferred in the goroutine and also stored in w.cancel for Stop()
	tickCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go func() {
		defer cancel()
		defer close(w.flushDone)
		w.flushLoop(tickCtx)
	}()

	w.routeBuf.start(ctx)

	w.logger.Info("store writer started",
		slog.Duration("flush_interval", w.cfg.FlushInterval),
		slog.Int("batch_size", w.cfg.BatchSize),
	)
	return nil
}

// Stop unsubscribes from all events, flushes any remaining pending
// updates, and stops the flush ticker.
func (w *Writer) Stop() error {
	if w.cancel != nil {
		w.cancel()
	}

	// Wait for flushLoop goroutine to exit.
	<-w.flushDone

	for _, sub := range w.subs {
		if err := w.bus.Unsubscribe(sub); err != nil {
			w.logger.Warn("failed to unsubscribe",
				slog.String("subscription_id", sub.ID),
				slog.String("error", err.Error()),
			)
		}
	}

	// Stop the route buffer and flush any remaining buffered points.
	w.routeBuf.stop()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	w.routeBuf.flushAll(shutdownCtx)

	// Final telemetry flush with a short deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w.flush(ctx)

	w.closeOnce.Do(func() { close(w.done) })

	w.logger.Info("store writer stopped")
	return nil
}

// Done returns a channel that is closed after Stop completes.
func (w *Writer) Done() <-chan struct{} {
	return w.done
}

// handleTelemetry is the event handler for VehicleTelemetryEvent. It
// extracts fields, maps them to a VehicleUpdate, and coalesces into
// the pending map. If the batch size is reached, it triggers a flush.
func (w *Writer) handleTelemetry(event events.Event) {
	telEvt, ok := event.Payload.(events.VehicleTelemetryEvent)
	if !ok {
		w.logger.Error("unexpected payload type for telemetry event",
			slog.String("event_id", event.ID),
		)
		return
	}

	update := mapTelemetryToUpdate(telEvt.Fields)
	if update == nil {
		return
	}
	update.LastUpdated = telEvt.CreatedAt

	shouldFlush := w.coalesce(telEvt.VIN, update)
	if shouldFlush {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		w.flush(ctx)
	}
}

// flushLoop runs a ticker that calls flush at each interval until the
// context is cancelled.
func (w *Writer) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.flush(ctx)
		}
	}
}

// flush drains the pending map and writes each VIN's coalesced update
// to the database. Errors are logged but do not halt the writer.
func (w *Writer) flush(ctx context.Context) {
	w.pendingMu.Lock()
	if len(w.pending) == 0 {
		w.pendingMu.Unlock()
		return
	}
	batch := w.pending
	w.pending = make(map[string]*VehicleUpdate)
	w.count = 0
	w.pendingMu.Unlock()

	w.logger.Debug("flushing telemetry batch",
		slog.Int("vehicles", len(batch)),
	)

	for vin, update := range batch {
		w.applyDestinationAddress(ctx, vin, update)
		if err := w.vehicles.UpdateTelemetry(ctx, vin, *update); err != nil {
			w.logger.Warn("failed to write telemetry update",
				slog.String("vin", redactVIN(vin)),
				slog.String("error", err.Error()),
			)
		}
	}
}

// applyDestinationAddress fills update.DestinationAddress when the flush
// is about to write a destination GPS pair and the cache lacks a fresh
// entry for it. Clears the cache when the navigation atomic group is
// being cleared (NFR-3.3 / vehicle-state-schema.md §3.1 all-or-nothing
// clear). On geocoder failure (timeout, no result, transport error) the
// address is left nil — the SDK falls back to the raw GPS pair (FR-3.4
// graceful degradation).
func (w *Writer) applyDestinationAddress(ctx context.Context, vin string, update *VehicleUpdate) {
	if clearsDestination(update.ClearFields) {
		w.invalidateDestAddr(vin)
		return
	}
	if update.DestinationLatitude == nil || update.DestinationLongitude == nil {
		return
	}
	if update.DestinationAddress != nil {
		// Caller (or a prior coalesced event) already supplied the
		// address — refresh the cache so a subsequent unchanged flush
		// short-circuits.
		w.cacheDestAddr(vin, *update.DestinationLatitude, *update.DestinationLongitude, *update.DestinationAddress)
		return
	}
	if cached, ok := w.lookupDestAddr(vin); ok &&
		cached.lat == *update.DestinationLatitude &&
		cached.lng == *update.DestinationLongitude {
		addr := cached.address
		update.DestinationAddress = &addr
		return
	}

	geoCtx, cancel := context.WithTimeout(ctx, destGeocodeTimeout)
	defer cancel()
	res, err := w.geocoder.ReverseGeocode(geoCtx, *update.DestinationLatitude, *update.DestinationLongitude)
	if err != nil {
		// ErrNoResult is the routine "geocoder returned nothing" path
		// (NoopGeocoder, or Mapbox legitimately had no match). Log the
		// transport / timeout cases at Warn so an operator chasing a
		// stuck navigation address has a breadcrumb.
		if !errors.Is(err, geocode.ErrNoResult) {
			w.logger.Warn("reverse geocode failed for destination",
				slog.String("vin", redactVIN(vin)),
				slog.String("error", err.Error()),
			)
		}
		return
	}
	addr := res.Address
	update.DestinationAddress = &addr
	w.cacheDestAddr(vin, *update.DestinationLatitude, *update.DestinationLongitude, addr)
}

// clearsDestination reports whether the flushed update is part of an
// active-navigation cancellation (FieldDestLocation Invalid clears
// destinationLatitude + destinationLongitude + destinationAddress per
// field_mapper.navFieldColumns).
func clearsDestination(cols []string) bool {
	for _, c := range cols {
		if c == "destinationLatitude" || c == "destinationLongitude" {
			return true
		}
	}
	return false
}

// lookupDestAddr returns the cached destination-address entry for vin,
// or false when there is no entry. Safe for concurrent use.
func (w *Writer) lookupDestAddr(vin string) (destAddrEntry, bool) {
	w.destAddrMu.Lock()
	defer w.destAddrMu.Unlock()
	e, ok := w.destAddrCache[vin]
	return e, ok
}

// cacheDestAddr stores a (lat, lng, address) tuple for vin so subsequent
// unchanged flushes skip the geocoder call. Safe for concurrent use.
func (w *Writer) cacheDestAddr(vin string, lat, lng float64, address string) {
	w.destAddrMu.Lock()
	defer w.destAddrMu.Unlock()
	w.destAddrCache[vin] = destAddrEntry{lat: lat, lng: lng, address: address}
}

// invalidateDestAddr drops vin from the cache so the next flush carrying
// destination GPS triggers a fresh geocoder call. Called on
// active-navigation cancellation. Safe for concurrent use.
func (w *Writer) invalidateDestAddr(vin string) {
	w.destAddrMu.Lock()
	defer w.destAddrMu.Unlock()
	delete(w.destAddrCache, vin)
}
