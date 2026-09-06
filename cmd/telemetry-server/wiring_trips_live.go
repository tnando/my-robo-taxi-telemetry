package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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

// tripLiveStoreAdapter adapts *store.TripLiveRepo onto trips.TripStore. Named
// apart from tripStoreAdapter (wiring_trips.go), which adapts *store.TripRepo
// onto the REST surface's telemetry.TripStore: two seams, two repositories, two
// vocabularies, and one composition root that can see all of it.
//
// The two TripAudience/TripVehicle shapes are converted field by field rather
// than shared, so a new column MUST be taught to this package instead of
// silently arriving as a zero value.
type tripLiveStoreAdapter struct{ repo *store.TripLiveRepo }

func (a *tripLiveStoreAdapter) TripAudienceFor(ctx context.Context, tripID string) (trips.TripAudience, error) {
	row, err := a.repo.TripAudienceFor(ctx, tripID)
	if err != nil {
		return trips.TripAudience{}, fmt.Errorf("trips: read audience: %w", err)
	}
	return trips.TripAudience{
		TripID:             row.TripID,
		VehicleID:          row.VehicleID,
		OwnerUserID:        row.OwnerUserID,
		ParticipantUserIDs: row.ParticipantUserIDs,
	}, nil
}

func (a *tripLiveStoreAdapter) TripNameFor(ctx context.Context, tripID string) (string, error) {
	name, err := a.repo.TripNameFor(ctx, tripID)
	if err != nil {
		return "", fmt.Errorf("trips: read name: %w", err)
	}
	return name, nil
}

func (a *tripLiveStoreAdapter) ClaimTripsToStart(ctx context.Context, limit int) ([]string, error) {
	ids, err := a.repo.ClaimTripsToStart(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("trips: claim starts: %w", err)
	}
	return ids, nil
}

func (a *tripLiveStoreAdapter) ClaimTripsToEnd(ctx context.Context, limit int) ([]string, error) {
	ids, err := a.repo.ClaimTripsToEnd(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("trips: claim endings: %w", err)
	}
	return ids, nil
}

func (a *tripLiveStoreAdapter) ClaimTripStartNow(ctx context.Context, tripID string) (bool, error) {
	claimed, err := a.repo.ClaimTripStartNow(ctx, tripID)
	if err != nil {
		return false, fmt.Errorf("trips: claim start: %w", err)
	}
	return claimed, nil
}

func (a *tripLiveStoreAdapter) ClaimTripEndNow(ctx context.Context, tripID string) (bool, error) {
	claimed, err := a.repo.ClaimTripEndNow(ctx, tripID)
	if err != nil {
		return false, fmt.Errorf("trips: claim end: %w", err)
	}
	return claimed, nil
}

func (a *tripLiveStoreAdapter) ActiveTripForVehicle(ctx context.Context, vehicleID string) (string, error) {
	tripID, err := a.repo.ActiveTripForVehicle(ctx, vehicleID)
	if err != nil {
		return "", fmt.Errorf("trips: confirm open window: %w", err)
	}
	return tripID, nil
}

func (a *tripLiveStoreAdapter) ActiveTripVehicles(ctx context.Context, limit int) ([]trips.TripVehicle, error) {
	rows, err := a.repo.ActiveTripVehicles(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("trips: list open windows: %w", err)
	}
	out := make([]trips.TripVehicle, 0, len(rows))
	for _, row := range rows {
		out = append(out, trips.TripVehicle{VehicleID: row.VehicleID, TripID: row.TripID})
	}
	return out, nil
}

// tripLegStoreAdapter adapts *store.TripLegRepo onto trips.LegStore.
type tripLegStoreAdapter struct{ repo *store.TripLegRepo }

func (a *tripLegStoreAdapter) StartLeg(
	ctx context.Context, tripID, vehicleID, destination string, startedAt time.Time,
) (trips.Leg, error) {
	row, err := a.repo.StartLeg(ctx, tripID, vehicleID, destination, startedAt)
	if err != nil {
		return trips.Leg{}, fmt.Errorf("trips: start leg: %w", err)
	}
	return legFromRow(&row), nil
}

func (a *tripLegStoreAdapter) EndLeg(ctx context.Context, legID string, endedAt time.Time, arrived bool) error {
	if err := a.repo.EndLeg(ctx, legID, endedAt, arrived); err != nil {
		return fmt.Errorf("trips: end leg: %w", err)
	}
	return nil
}

func (a *tripLegStoreAdapter) OpenLegForVehicle(ctx context.Context, vehicleID string) (trips.Leg, error) {
	row, err := a.repo.OpenLegForVehicle(ctx, vehicleID)
	if err != nil {
		return trips.Leg{}, fmt.Errorf("trips: read open leg: %w", err)
	}
	return legFromRow(&row), nil
}

func (a *tripLegStoreAdapter) OpenLegsForTrip(ctx context.Context, tripID string) ([]trips.Leg, error) {
	rows, err := a.repo.OpenLegsForTrip(ctx, tripID)
	if err != nil {
		return nil, fmt.Errorf("trips: list open legs: %w", err)
	}
	out := make([]trips.Leg, 0, len(rows))
	// Indexed rather than ranged by value: store.TripLeg is wide enough that
	// gocritic flags the per-iteration copy, the same note activityFromRow's
	// loops carry.
	for i := range rows {
		out = append(out, legFromRow(&rows[i]))
	}
	return out, nil
}

func (a *tripLegStoreAdapter) ClaimLegStartedPush(ctx context.Context, legID string) (bool, error) {
	return wrapClaim(a.repo.ClaimLegStartedPush(ctx, legID))
}

func (a *tripLegStoreAdapter) ClaimLegArrivedPush(ctx context.Context, legID string) (bool, error) {
	return wrapClaim(a.repo.ClaimLegArrivedPush(ctx, legID))
}

func (a *tripLegStoreAdapter) ClaimLegActivityStart(ctx context.Context, legID string) (bool, error) {
	return wrapClaim(a.repo.ClaimLegActivityStart(ctx, legID))
}

func (a *tripLegStoreAdapter) ClaimLegActivityEnd(ctx context.Context, legID string) (bool, error) {
	return wrapClaim(a.repo.ClaimLegActivityEnd(ctx, legID))
}

// wrapClaim adds the package prefix to a claim's error. The four claims differ
// only in their statement, so their error wrapping is written once.
func wrapClaim(claimed bool, err error) (bool, error) {
	if err != nil {
		return false, fmt.Errorf("trips: claim: %w", err)
	}
	return claimed, nil
}

// legFromRow converts a store row to the trips package's own shape. Written out
// field by field, like activityFromRow, so a new column must be taught here.
func legFromRow(row *store.TripLeg) trips.Leg {
	return trips.Leg{
		ID:              row.ID,
		TripID:          row.TripID,
		VehicleID:       row.VehicleID,
		DestinationName: row.DestinationName,
		StartedAt:       row.StartedAt,
		EndedAt:         row.EndedAt,
	}
}

// tripActivityStoreAdapter joins the TWO registries a leg's card needs onto one
// interface: the push-to-start tokens and the per-Activity update tokens.
//
// They are two tables and two meanings of a 410 — see
// internal/store/trip_activity_token_repo.go — which is exactly why they are
// adapted together HERE rather than passed as one repo: this is the only place
// that knows both, and it is where the routing of each rejection is decided.
type tripActivityStoreAdapter struct {
	tokens     *store.TripActivityTokenRepo
	activities *store.LiveActivityRepo
}

func (a *tripActivityStoreAdapter) PushToStartTokensForTrip(
	ctx context.Context, tripID string,
) ([]push.ActivityStartToken, error) {
	rows, err := a.tokens.PushToStartTokensForTrip(ctx, tripID)
	if err != nil {
		return nil, fmt.Errorf("trips: list push-to-start tokens: %w", err)
	}
	out := make([]push.ActivityStartToken, 0, len(rows))
	for _, row := range rows {
		out = append(out, push.ActivityStartToken{
			UserID: row.UserID, Token: row.PushToStartToken, Sandbox: row.Sandbox,
		})
	}
	return out, nil
}

func (a *tripActivityStoreAdapter) DeleteRejectedPushToStartToken(ctx context.Context, token string) error {
	if err := a.tokens.DeleteRejectedPushToStartToken(ctx, token); err != nil {
		return fmt.Errorf("trips: delete rejected push-to-start token: %w", err)
	}
	return nil
}

func (a *tripActivityStoreAdapter) ActivitiesForLeg(ctx context.Context, legID string) ([]push.Activity, error) {
	rows, err := a.activities.ActivitiesForLeg(ctx, legID)
	if err != nil {
		return nil, fmt.Errorf("trips: list leg activities: %w", err)
	}
	out := make([]push.Activity, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		out = append(out, push.Activity{
			// THE LEG ANCHOR, in its own field. It used to ride the
			// RideRequestID slot on the argument that nothing read it — see
			// push.Activity.TripLegID for why that stopped being good enough.
			TripLegID: row.TripLegID,
			UserID:    row.UserID,
			Token:     row.ActivityPushToken,
			Sandbox:   row.Sandbox,
		})
	}
	return out, nil
}

func (a *tripActivityStoreAdapter) EndActivitiesForLeg(ctx context.Context, legID string) (int64, error) {
	n, err := a.activities.EndActivitiesForLeg(ctx, legID)
	if err != nil {
		return 0, fmt.Errorf("trips: end leg activities: %w", err)
	}
	return n, nil
}

func (a *tripActivityStoreAdapter) MarkLegActivitiesPushed(
	ctx context.Context, legID string, userIDs []string,
) (int64, error) {
	n, err := a.activities.MarkLegActivitiesPushed(ctx, legID, userIDs)
	if err != nil {
		return 0, fmt.Errorf("trips: mark leg activities pushed: %w", err)
	}
	return n, nil
}

func (a *tripActivityStoreAdapter) DeleteActivityToken(ctx context.Context, token string) error {
	if err := a.activities.DeleteActivityToken(ctx, token); err != nil {
		return fmt.Errorf("trips: delete rejected activity token: %w", err)
	}
	return nil
}
