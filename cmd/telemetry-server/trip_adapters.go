package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// ─────────────────────────────────────────────────────────────────────────────
// MYR-602 TRIPS — boundary translation. BEGIN
// ─────────────────────────────────────────────────────────────────────────────
//
// Shape conversion and error translation between internal/store and
// internal/telemetry, split from wiring_trips.go under the 300-line cap.

// translateTripError maps the store's sentinels onto the handler package's.
//
// EXHAUSTIVE OVER THE STORE'S TRIP SENTINELS, and that is checked by
// TestTranslateTripErrorCoversEveryStoreSentinel rather than by care. An
// unmapped sentinel would fall through to the default arm and be reported as
// 500 — a client told to retry a request that will never succeed.
//
// The ORIGINAL error is preserved in the chain (%w on both sides) so the
// server-side log still names the underlying cause, while the handler branches
// on its own sentinel. Nothing here reads or copies a MESSAGE: the store's text
// is not a wire contract, and the handler writes its own.
func translateTripError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, store.ErrTripNotFound):
		return fmt.Errorf("%w: %w", telemetry.ErrTripNotFound, err)
	case errors.Is(err, store.ErrTripOverlap):
		return fmt.Errorf("%w: %w", telemetry.ErrTripOverlaps, err)
	case errors.Is(err, store.ErrTripParticipantNotShared):
		return fmt.Errorf("%w: %w", telemetry.ErrTripParticipantNotShared, err)
	case errors.Is(err, store.ErrTripWindowInvalid):
		return fmt.Errorf("%w: %w", telemetry.ErrTripWindowInvalid, err)
	case errors.Is(err, store.ErrTripNameInvalid):
		return fmt.Errorf("%w: %w", telemetry.ErrTripNameInvalid, err)
	case errors.Is(err, store.ErrTripEnded):
		return fmt.Errorf("%w: %w", telemetry.ErrTripEnded, err)
	default:
		// A transport failure or a bug. Passed through unchanged so the
		// handler's default arm reports 500 and logs it — never dressed as one
		// of the refusals above, which would tell a client to change a request
		// that was fine.
		return err
	}
}

// tripData converts a store TripView to the handler's shape.
func tripData(v store.TripView) telemetry.TripData {
	participants := make([]telemetry.TripParticipantData, 0, len(v.Participants))
	for _, p := range v.Participants {
		participants = append(participants, telemetry.TripParticipantData{
			ParticipantID: p.ParticipantID,
			Name:          p.Name,
			UserID:        p.UserID,
		})
	}

	out := telemetry.TripData{
		ID:             v.ID,
		VehicleID:      v.VehicleID,
		Name:           v.Name,
		StartsAt:       v.StartsAt,
		EndsAt:         v.EndsAt,
		EndedAt:        v.EndedAt,
		CreatedAt:      v.CreatedAt,
		Role:           v.Role,
		OwnerFirstName: v.OwnerFirstName,
		Vehicle: telemetry.TripVehicleData{
			VehicleID: v.Vehicle.VehicleID,
			Name:      v.Vehicle.Name,
			Model:     v.Vehicle.Model,
			Year:      v.Vehicle.Year,
			Color:     v.Vehicle.Color,
			VIN:       v.Vehicle.VIN,
			TrimLabel: v.Vehicle.TrimLabel,
			Trim:      v.Vehicle.Trim,
		},
		Participants: participants,
		DriveCount:   v.DriveCount,
	}
	// THE SWEEPER'S STAMPS ARE DELIBERATELY NOT CARRIED. `started_notified_at`
	// and `ended_notified_at` record what was NOTIFIED, never what is TRUE, and
	// a wire consumer that read one would derive a status from a delivery
	// receipt. Status is derived from the instants, on every read.
	if v.CurrentLeg != nil {
		out.CurrentLeg = &telemetry.TripLegData{
			DestinationName: v.CurrentLeg.DestinationName,
			EtaMinutes:      v.CurrentLeg.EtaMinutes,
			StartedAt:       v.CurrentLeg.StartedAt,
		}
	}
	return out
}

// driveListPage converts the store's drive page to the handler's, reusing the
// SAME item conversion §7.2's adapter uses so a drive cannot render one way in
// a trip and another way in the car's own history.
func driveListPage(page store.DriveListPage) telemetry.DriveListPage {
	items := make([]telemetry.DriveListItem, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, driveListItem(page.Items[i]))
	}
	return telemetry.DriveListPage{Items: items, HasMore: page.HasMore}
}

// tripDriveAdmitterAdapter is the MYR-602 window gate for §7.2/§7.3/§7.4.
type tripDriveAdmitterAdapter struct{ repo *store.TripRepo }

func (a *tripDriveAdmitterAdapter) TripDriveWindows(ctx context.Context, userID, vehicleID string) ([]telemetry.TripDriveWindow, error) {
	windows, err := a.repo.TripDriveWindows(ctx, userID, vehicleID)
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.TripDriveWindow, 0, len(windows))
	for _, w := range windows {
		out = append(out, telemetry.TripDriveWindow{From: w.From, To: w.To})
	}
	return out, nil
}

func (a *tripDriveAdmitterAdapter) VehicleDrivesInTripWindows(
	ctx context.Context, userID, vehicleID string, cursor telemetry.DriveListCursor, limit int,
) (telemetry.DriveListPage, error) {
	page, err := a.repo.VehicleDrivesInTripWindows(ctx, userID, vehicleID,
		store.DriveListCursor{StartTime: cursor.StartTime, ID: cursor.ID}, limit)
	if err != nil {
		return telemetry.DriveListPage{}, err
	}
	return driveListPage(page), nil
}

// tripVehicleListerAdapter is the catalog's third merge leg.
type tripVehicleListerAdapter struct {
	repo *store.TripRepo
	// shared is the SAME adapter the MYR-184 viewer merge uses. Reused rather
	// than reimplemented so a trip row and a share row for the same car are
	// built by one access-checked query and cannot describe it differently —
	// and so the share join that makes trip access unable to outlive the share
	// is applied here too, by the query that already carries it.
	shared *sharedVehicleListerAdapter
}

// ListTripVehiclesByUser returns the catalog rows for vehicles the caller
// reaches ONLY through an open window.
//
// It resolves the ids through the trip repository and then reads the rows
// through the VEHICLE repository's own shared-catalog projection — rather than
// selecting catalog columns in a trip statement — so a trip row and a share row
// for the same car are built by the same query and cannot describe it
// differently.
func (a *tripVehicleListerAdapter) ListTripVehiclesByUser(ctx context.Context, userID string) ([]telemetry.TripVehicleRow, error) {
	byVehicle, err := a.repo.ActiveTripVehicleIDs(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(byVehicle) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(byVehicle))
	for id := range byVehicle {
		ids = append(ids, id)
	}

	rows, err := a.shared.ListSharedByIDs(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.TripVehicleRow, 0, len(rows))
	for i := range rows {
		out = append(out, telemetry.TripVehicleRow{
			VehicleCatalogRow: rows[i].VehicleCatalogRow,
			TripID:            byVehicle[rows[i].ID],
			// The caller's OWN ride capability, carried through so the trip
			// row's `sharePermission` matches the share row it replaces.
			AllowRides: rows[i].AllowRides,
		})
	}
	return out, nil
}

func (a *tripVehicleListerAdapter) ActiveTripIDsByUser(ctx context.Context, userID string) (map[string]string, error) {
	return a.repo.ActiveTripVehicleIDs(ctx, userID)
}

// ─────────────────────────────────────────────────────────────────────────────
// MYR-602 TRIPS — boundary translation. END
// ─────────────────────────────────────────────────────────────────────────────
