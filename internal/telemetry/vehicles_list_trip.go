package telemetry

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// The TRIP MERGE for GET /api/vehicles (MYR-602) — the THIRD non-owner leg,
// after MYR-184's shares and MYR-540's ride members.
//
// A trip participant holds live access to the car for the length of the window
// (internal/auth's fourth UNION leg admits them to the socket), but the CATALOG
// is what the app's watched-vehicle ladder resolves against, and a car the
// catalog omits is a car the app treats as gone. This file appends the vehicles
// of the caller's OPEN windows so the three surfaces — catalog, access set,
// role resolution — name the same cars for the same caller.
//
// TWO THINGS THIS FILE DOES, and they are separate on purpose:
//
//  1. It ADDS ROWS for cars the caller reaches only through a trip.
//  2. It ANNOTATES rows that are already there — owner rows and share rows —
//     with `activeTripId`, because an owner's own car and a car already shared
//     with the caller are exactly the cars a trip is most often opened on, and
//     those rows arrive through the earlier legs.
//
// The added rows are projected through the `trip_participant` MASK — the viewer
// field set PLUS the location and navigation groups — while carrying the WIRE
// role `viewer`, which is what wireRole projects every non-owner onto. Same
// split, same reason, as the member merge: the mask decides the FIELDS, the
// wire enum stays the closed two-value one every shipped client decodes.

// TripVehicleLister returns the catalog rows and trip ids for the caller's open
// windows.
//
// Implementations apply the live-share join themselves — the join IS the access
// check — so a participant whose grant was revoked yields no row, and trip
// access cannot outlive the share it was picked from.
type TripVehicleLister interface {
	// ListTripVehiclesByUser returns viewer-shaped rows for vehicles the
	// caller reaches through an OPEN trip window, each paired with that
	// trip's id.
	ListTripVehiclesByUser(ctx context.Context, userID string) ([]TripVehicleRow, error)

	// ActiveTripIDsByUser returns vehicleID → tripID for EVERY open window the
	// caller is party to, INCLUDING the ones on cars they own.
	//
	// A second method rather than a field on the rows above, because the two
	// answer different questions: that one is "which cars does a trip ADD to
	// your catalog", this one is "which of the cars in your catalog — however
	// they got there — have a window open right now". An owner's own car is in
	// the second set and never in the first.
	ActiveTripIDsByUser(ctx context.Context, userID string) (map[string]string, error)
}

// TripVehicleRow is a catalog row plus the id of the trip that admits it.
type TripVehicleRow struct {
	VehicleCatalogRow
	TripID string
}

// WithTripVehicles enables the trip merge. Omitting it leaves the endpoint
// owner+shared+member — the pre-MYR-602 behaviour — which is the fail-closed
// direction: a deployment that forgot to wire trips under-reports rather than
// over-shares.
func WithTripVehicles(trips TripVehicleLister) VehiclesListOption {
	return func(h *VehiclesListHandler) {
		h.tripVehicles = trips
	}
}

// appendTripRows adds the trip-only vehicles and stamps `activeTripId` on every
// row a window is open on.
//
// FAILURES ARE LOGGED AND SWALLOWED, like the two merges before it. The
// degraded response is a strictly smaller set — never a wider one — so the
// failure mode is "a trip's car is missing for a moment", not "a car appears
// that should not". An owner's own garage must not go down because the trip
// join hiccuped.
func (h *VehiclesListHandler) appendTripRows(ctx context.Context, userID string, resp vehiclesListResponse) vehiclesListResponse {
	if h.tripVehicles == nil {
		return resp
	}

	// THE ANNOTATION RUNS FIRST AND INDEPENDENTLY of the row merge, so a
	// failure in one does not take the other with it: an owner can lose the
	// added rows and keep `activeTripId` on their own car, which is the row
	// that matters most to them.
	activeByVehicle, err := h.tripVehicles.ActiveTripIDsByUser(ctx, userID)
	if err != nil {
		h.logger.Error("vehicles list: active-trip lookup failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		activeByVehicle = nil
	}

	rows, err := h.tripVehicles.ListTripVehiclesByUser(ctx, userID)
	if err != nil {
		h.logger.Error("vehicles list: trip-vehicle merge failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		rows = nil
	}

	// DEDUPE KEEPS THE ROW ALREADY IN HAND, exactly as the member merge does.
	// An owner row carries the owner mask and a share row carries a real
	// capability; both say strictly more about the same car than a trip
	// membership does. The trip row appears only when the window is the ONLY
	// way the caller reaches the car — which cannot actually happen today,
	// since every participant is by construction a share-holder and the share
	// leg runs first, but the merge is written to be correct rather than to
	// depend on that. If shares and trips ever come apart, this leg is what
	// keeps the catalog honest instead of silently dropping a car.
	seen := make(map[string]bool, len(resp.Items))
	for _, item := range resp.Items {
		if id, ok := item["vehicleId"].(string); ok {
			seen[id] = true
		}
	}

	// One clock reading for the appended rows, matching the other three legs:
	// every row of one catalog response judges its MYR-491 setup state against
	// the same instant.
	now := time.Now()
	for i := range rows {
		if seen[rows[i].ID] {
			continue
		}
		seen[rows[i].ID] = true
		resp.Items = append(resp.Items, nonOwnerSummaryMap(
			rows[i].VehicleCatalogRow, auth.ShareGrant{}, now, auth.RoleTripParticipant,
		))
	}

	// STAMPED ON EVERY ROW, whichever leg produced it. `activeTripId` is how a
	// client knows it may watch the car — the server resolves such a caller to
	// the trip_participant mask, and there is no wire role that says so — and
	// an owner reads their own row's value to render the trip card. A row with
	// no open window keeps the key ABSENT rather than null: the contract marks
	// the field optional, and absence is the one spelling a pre-v0.41.0 server
	// also produces, so a client that has to handle absence anyway handles
	// both.
	for _, item := range resp.Items {
		id, ok := item["vehicleId"].(string)
		if !ok {
			continue
		}
		tripID := activeByVehicle[id]
		if tripID == "" {
			continue
		}
		// THE STAMP ASKS THE MASK FIRST. These rows have already been
		// projected by the leg that produced them, so there is no unprojected
		// map left to run through Apply — and writing a key onto a projected
		// row is precisely the bypass the mask layer exists to prevent. The
		// allow-list therefore stays the single authority: drop `activeTripId`
		// from a role's list and this stops writing it.
		//
		// The narrowest role is the one consulted. A non-owner row's WIRE role
		// is always `viewer` (wireRole is total), while its MASK role may have
		// been the elevated `ride_member` or `trip_participant` — and those two
		// lists are supersets of the viewer list by construction, so a field
		// the viewer list permits is permitted to all three. Checking the
		// narrowest is the conservative test, and
		// TestActiveTripIDIsAllowedOnEveryCatalogRole pins that the three
		// really do agree.
		role := auth.RoleViewer
		if wire, _ := item["role"].(string); wire == string(auth.RoleOwner) {
			role = auth.RoleOwner
		}
		if !mask.For(mask.ResourceVehicleSummary, role).Allows(activeTripIDField) {
			continue
		}
		item[activeTripIDField] = tripID
	}
	return resp
}

// activeTripIDField is the wire key, named once so the mask consultation and
// the write cannot drift onto two different strings.
const activeTripIDField = "activeTripId"
