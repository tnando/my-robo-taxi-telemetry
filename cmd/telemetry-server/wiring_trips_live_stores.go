package main

import (
	"context"
	"fmt"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/trips"
)

// THE TWO STORE ADAPTERS the live half runs on, bound to their repositories.
//
// Split from wiring_trips_live.go so both stay inside the 300-line cap, and the
// seam is the one the file already had: that file DECIDES what to build and
// whether to build it at all (the kill switch, the shared APNs client, the
// lifecycle), while these are pure translations with no decision in them.
//
// EVERY SHAPE IS CONVERTED FIELD BY FIELD rather than shared, which is
// deliberate and is the whole reason these are worth reading: a new column has
// to be TAUGHT to internal/trips instead of silently arriving there as a zero
// value.

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
