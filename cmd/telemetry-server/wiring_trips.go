package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// ─────────────────────────────────────────────────────────────────────────────
// MYR-602 TRIPS — composition root. BEGIN
// ─────────────────────────────────────────────────────────────────────────────

// setupTripEndpoints mounts the trips surface (rest-api.md §7.30):
//
//	POST   /api/vehicles/{vehicleId}/trips
//	GET    /api/trips
//	GET    /api/trips/{tripId}
//	PATCH  /api/trips/{tripId}
//	POST   /api/trips/{tripId}/end
//	DELETE /api/trips/{tripId}/participants/me
//	GET    /api/trips/{tripId}/drives
//	POST   /api/trips/{tripId}/activity-start-token
//	DELETE /api/trips/{tripId}/activity-start-token
//	POST   /api/trips/{tripId}/legs/{legId}/activity-token
//	DELETE /api/trips/{tripId}/legs/{legId}/activity-token
//
// The last PAIR is the LEG anchor of §7.21's per-Activity path, and it is the
// other half of push-to-start: §7.30.8 registers the token that lets the server
// CREATE a card, and these file the per-Activity token that addresses the card
// once it exists. Without them the server can raise a leg's card and never
// update or end it.
//
// ALWAYS MOUNTED. The kill switch is passed INTO the handler rather than
// deciding whether to register the routes, and the difference matters: an
// unmounted route is a 404, which tells a client the feature does not exist and
// which some clients cache. A mounted route with the switch off is a 503, which
// says "not right now" — the true thing.
//
// The encryptor is NOT optional: go_trips stores the trip name encrypt-only
// (NOT NULL, no plaintext sibling), so NewTripRepo panics on nil rather than
// letting a deployment write P1 user content in the clear or fail every write
// at the constraint.
func setupTripEndpoints(
	deps httpRouteDeps,
	snapshotAdapter telemetry.VehicleSnapshotReader,
	notifier telemetry.TripNotifier,
) *store.TripRepo {
	logger := deps.logger.With(slog.String("component", "trips"))

	repo := store.NewTripRepo(deps.pool, deps.storeMetrics, deps.encryptor, logger)

	// The notifier is the seam onto internal/trips (see trip_notifier_adapter.go).
	// A TYPED NIL IS NOT AN ABSENT INTERFACE, so the option is only applied when
	// there is genuinely something behind it — otherwise the handler's own
	// noopTripNotifier would be replaced by a non-nil interface holding nil and
	// every announcement would panic after a perfectly good commit.
	opts := []telemetry.TripOption{}
	if notifier != nil {
		opts = append(opts, telemetry.WithTripNotifier(notifier))
	}

	handler := telemetry.NewTripHandler(
		deps.authenticator,
		&tripStoreAdapter{repo: repo, activities: deps.liveActivityRepo},
		snapshotAdapter,
		deps.cfg.TripsEnabled(),
		logger,
		opts...,
	)

	deps.srv.HandleFunc("POST /api/vehicles/{vehicleId}/trips", handler.ServeCreate)
	deps.srv.HandleFunc("GET /api/trips", handler.ServeList)
	deps.srv.HandleFunc("GET /api/trips/{tripId}", handler.ServeGet)
	deps.srv.HandleFunc("PATCH /api/trips/{tripId}", handler.ServePatch)
	deps.srv.HandleFunc("POST /api/trips/{tripId}/end", handler.ServeEnd)
	deps.srv.HandleFunc("DELETE /api/trips/{tripId}/participants/me", handler.ServeLeave)
	deps.srv.HandleFunc("GET /api/trips/{tripId}/drives", handler.ServeDrives)
	deps.srv.HandleFunc("POST /api/trips/{tripId}/activity-start-token", handler.ServeRegisterActivityToken)
	deps.srv.HandleFunc("DELETE /api/trips/{tripId}/activity-start-token", handler.ServeDeleteActivityToken)
	deps.srv.HandleFunc("POST /api/trips/{tripId}/legs/{legId}/activity-token", handler.ServeRegisterLegActivityToken)
	deps.srv.HandleFunc("DELETE /api/trips/{tripId}/legs/{legId}/activity-token", handler.ServeEndLegActivityToken)

	logger.Info("trip endpoints enabled (§7.30)",
		slog.Int("routes", 11),
		slog.Bool("feature_enabled", deps.cfg.TripsEnabled()))

	// RETURNED, not discarded: the same repository is the drives handlers'
	// window gate and the catalog's third merge leg. One instance rather than
	// three, so the three surfaces resolve a window through one statement set
	// and cannot come to disagree about who is on a trip.
	return repo
}

// tripStoreAdapter maps *store.TripRepo onto telemetry.TripStore, so
// internal/telemetry never imports internal/store (the dependency rule).
//
// It is also where the STORE'S ERROR SENTINELS are translated into the
// HANDLER'S. The two sets are deliberately separate — the handler package
// cannot see the store's — and this is the one place they meet, so a store
// error that reaches a client has been through exactly one mapping.
type tripStoreAdapter struct {
	repo *store.TripRepo
	// activities is the go_live_activities registry, reached by the two LEG
	// token routes only. The SAME instance the ride token endpoints and the
	// Live Activity sender use, so a card cannot be registered in one place and
	// looked for in another.
	activities *store.LiveActivityRepo
}

func (a *tripStoreAdapter) CreateTrip(ctx context.Context, in telemetry.TripCreateInput) (telemetry.TripData, error) {
	view, err := a.repo.Create(ctx, store.CreateTripInput{
		VehicleID:           in.VehicleID,
		OwnerUserID:         in.OwnerUserID,
		Name:                in.Name,
		StartsAt:            in.StartsAt,
		EndsAt:              in.EndsAt,
		ParticipantShareIDs: in.ParticipantShareIDs,
	})
	return tripData(view), translateTripError(err)
}

func (a *tripStoreAdapter) GetTrip(ctx context.Context, tripID, userID string) (telemetry.TripData, error) {
	view, err := a.repo.GetForUser(ctx, tripID, userID)
	return tripData(view), translateTripError(err)
}

func (a *tripStoreAdapter) ListTrips(ctx context.Context, userID, status string, limit int) ([]telemetry.TripData, error) {
	views, err := a.repo.ListForUser(ctx, userID, store.TripStatus(status), limit)
	if err != nil {
		return nil, translateTripError(err)
	}
	out := make([]telemetry.TripData, 0, len(views))
	for i := range views {
		out = append(out, tripData(views[i]))
	}
	return out, nil
}

func (a *tripStoreAdapter) UpdateTrip(ctx context.Context, tripID, ownerUserID string, in telemetry.TripUpdateInput) (telemetry.TripData, error) {
	view, err := a.repo.Update(ctx, tripID, ownerUserID, store.UpdateTripInput{
		Name:                 in.Name,
		EndsAt:               in.EndsAt,
		AddParticipantIDs:    in.AddParticipantIDs,
		RemoveParticipantIDs: in.RemoveParticipantIDs,
	})
	return tripData(view), translateTripError(err)
}

func (a *tripStoreAdapter) EndTrip(ctx context.Context, tripID, ownerUserID string) (telemetry.TripData, error) {
	view, err := a.repo.End(ctx, tripID, ownerUserID)
	return tripData(view), translateTripError(err)
}

func (a *tripStoreAdapter) LeaveTrip(ctx context.Context, tripID, userID string) error {
	_, err := a.repo.Leave(ctx, tripID, userID)
	return translateTripError(err)
}

func (a *tripStoreAdapter) TripDrives(
	ctx context.Context, tripID, userID string, cursor telemetry.DriveListCursor, limit int,
) (telemetry.DriveListPage, error) {
	page, err := a.repo.TripDrivesForUser(ctx, tripID, userID,
		store.DriveListCursor{StartTime: cursor.StartTime, ID: cursor.ID}, limit)
	if err != nil {
		return telemetry.DriveListPage{}, translateTripError(err)
	}
	return driveListPage(page), nil
}

func (a *tripStoreAdapter) RegisterTripActivityStartToken(ctx context.Context, tripID, userID, token string, sandbox bool) error {
	return translateTripError(a.repo.RegisterActivityStartToken(ctx, tripID, userID, token, sandbox))
}

func (a *tripStoreAdapter) DeleteTripActivityStartToken(ctx context.Context, tripID, userID string) error {
	return translateTripError(a.repo.DeleteActivityStartToken(ctx, tripID, userID))
}

// The LEG anchor of §7.21's per-Activity path (§7.30.10 / §7.30.11). It reaches
// a DIFFERENT repository from everything above — go_live_activities rather than
// go_trips — because the two tokens are two different things: the start token
// is the app's capability to create a card, this one addresses one card that
// already exists.
func (a *tripStoreAdapter) RegisterTripLegActivityToken(
	ctx context.Context, tripID, legID, userID, token string, sandbox bool,
) error {
	if a.activities == nil {
		return errLegActivityRegistryUnwired
	}
	return translateTripError(a.activities.RegisterLegActivity(ctx, tripID, legID, userID, token, sandbox))
}

func (a *tripStoreAdapter) EndTripLegActivityToken(ctx context.Context, legID, userID string) (bool, error) {
	if a.activities == nil {
		return false, errLegActivityRegistryUnwired
	}
	ended, err := a.activities.EndLegActivity(ctx, legID, userID)
	return ended, translateTripError(err)
}

// errLegActivityRegistryUnwired is a DEPLOYMENT error, not a runtime state: the
// registry is the same *store.LiveActivityRepo the ride path has always had, so
// a nil here means the composition root skipped a line. It falls through
// writeTripError's default arm to a logged 500, which is the honest answer —
// nothing the caller can change would help.
var errLegActivityRegistryUnwired = errors.New("trips: live activity registry not wired")

// ─────────────────────────────────────────────────────────────────────────────
// MYR-602 TRIPS — composition root. END
// ─────────────────────────────────────────────────────────────────────────────
