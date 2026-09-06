package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/push"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/trips"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// Live-trips wiring (MYR-602): the window sweeper, the leg detector, and the
// Live Activity sender that serves them both.
//
// It lives at cmd/ for the same dependency-rule reason the auto-arrival wiring
// does: internal/trips depends only on small consumer-site seams, and cmd/ is
// the only layer that can see internal/store, internal/push, internal/ws and
// internal/trips at once — which is what lets the adapters reuse the EXACT
// repositories and the EXACT revalidator the rest of the service uses rather
// than second implementations shaped like them.

// tripsLiveDeps is everything the live half needs from the composition root.
//
// A struct rather than nine parameters, and grouped by WHAT IT IS rather than
// by which component consumes it: the sweeper and the detector share every one
// of these, which is the point of Service holding them together.
type tripsLiveDeps struct {
	cfg          *config.Config
	bus          events.Bus
	tripRepo     *store.TripLiveRepo
	legRepo      *store.TripLegRepo
	tokenRepo    *store.TripActivityTokenRepo
	activityRepo *store.LiveActivityRepo
	prefsRepo    *store.PushPrefsRepo
	// names is the SAME nickname resolver the ride pushes use, so a car cannot
	// be called two different things on two surfaces in the same second.
	names       *store.VehicleNameRepo
	vins        *store.VINCache
	apns        *push.Client
	pusher      *push.Notifier
	revalidator *ws.AccessRevalidator
	logger      *slog.Logger
}

// tripsLive is what the composition root holds onto: the pieces it must stop,
// and the notifier the REST lane calls.
type tripsLive struct {
	// Service is what the trips HTTP handlers reach for trip_added,
	// trip_started and trip_ended — the last of which is SettleTrip under
	// another name. The CONCRETE type rather than trips.TripNotifier, so the
	// composition root can tell "no service" from "a nil service behind a
	// non-nil interface"; trip_notifier_adapter.go explains why that
	// distinction is worth a field type. Nil when TRIPS_ENABLED is false,
	// which the handler lane treats as "no notifications" rather than as an
	// error: the endpoints' own 503 is the kill switch's user-facing half.
	Service *trips.Service
	// Detector must be stopped on shutdown so its telemetry subscription is
	// released before the bus drains.
	Detector *trips.Detector
}

// setupTripsLive builds and starts the live half, or does nothing when
// TRIPS_ENABLED is false.
//
// ORDERING NOTE. The sweeper is started with a bare `go Run(ctx)` and no drain,
// matching the reservation sweeper and the Live Activity ticker: a pass
// interrupted mid-way costs at most one interval of latency on a window edge,
// and the next process re-claims the same rows a minute later because the
// claims are keyed on stamps rather than on anything in memory.
func setupTripsLive(ctx context.Context, deps tripsLiveDeps) (*tripsLive, error) {
	log := deps.logger.With(slog.String("component", "trips"))

	if !deps.cfg.TripsEnabled() {
		log.Info("trips disabled by TRIPS_ENABLED; no window sweeper, no leg detector, " +
			"and the trip endpoints answer 503")
		return &tripsLive{}, nil
	}

	// The APNs client is REUSED, never re-minted. The provider JWT is cached
	// per client with a 40-minute TTL and Apple throttles re-mints at 20
	// minutes, so a second client on one key is a way to get 403
	// ExpiredProviderToken under load for no benefit — the same argument
	// setupLiveActivityNotifier makes about the ride card.
	var activitySender push.ActivitySender
	if deps.apns != nil {
		// Typed-nil care: the keyless path keys off the INTERFACE being nil,
		// which a typed nil pointer is not.
		activitySender = deps.apns
	}
	tripActivities := push.NewTripActivityNotifier(
		activitySender,
		&tripActivityStoreAdapter{tokens: deps.tokenRepo, activities: deps.activityRepo},
		&pushPrefsAdapter{repo: deps.prefsRepo},
		push.Config{Enabled: deps.cfg.Push().Enabled},
		log.With(slog.String("surface", "live-activity")),
	)

	svc := trips.NewService(
		&tripLiveStoreAdapter{repo: deps.tripRepo},
		&tripLegStoreAdapter{repo: deps.legRepo},
		trips.DefaultConfig(),
		log,
	).
		WithActivities(tripActivities).
		WithVehicleNames(deps.names)

	// Both of these are OPTIONAL by construction and the typed-nil rule applies
	// to each: a deployment with no APNs credentials builds no *push.Notifier,
	// and a test wiring may build no revalidator.
	if deps.pusher != nil {
		svc = svc.WithPushes(deps.pusher)
	}
	if deps.revalidator != nil {
		svc = svc.WithRevalidator(deps.revalidator)
	}

	go trips.NewSweeper(svc, log.With(slog.String("surface", "sweeper"))).Run(ctx)

	detector := trips.NewDetector(svc, deps.bus, deps.vins,
		log.With(slog.String("surface", "leg-detector")))
	if err := detector.Start(ctx); err != nil {
		return nil, fmt.Errorf("starting trip leg detector: %w", err)
	}

	return &tripsLive{Service: svc, Detector: detector}, nil
}

// Stop releases the detector's subscription and waits out any detached re-mask
// sweep. Safe on a zero value, which is what the kill-switch-off path returns.
//
// THE DRAIN IS NOT TIDINESS. A trip transition's re-mask nudge runs on a
// context deliberately detached from the caller's (an HTTP handler's request
// context dies the moment it answers), so at shutdown there may be a sweep in
// flight that nothing else is waiting for. Abandoning it mid-pass would leave
// some sessions re-masked and others not — which on a closing window means
// somebody keeps live location until they reconnect.
func (t *tripsLive) Stop() {
	if t == nil {
		return
	}
	if t.Detector != nil {
		_ = t.Detector.Stop()
	}
	if t.Service != nil {
		t.Service.DrainRevalidation()
	}
}
