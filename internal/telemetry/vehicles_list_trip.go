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

// TripVehicleRow is a catalog row, the id of the trip that admits it, and the
// caller's ride capability on the underlying share.
type TripVehicleRow struct {
	VehicleCatalogRow
	TripID string

	// AllowRides is the caller's OWN share capability, carried because a trip
	// row REPLACES the share row for the same car (see appendTripRows) and the
	// replacement must not silently downgrade `sharePermission`. The trip
	// elevates the ROLE; it does not replace the relationship, and the grant
	// travels with the person.
	AllowRides bool
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

	// ⚠ THIS LEG UPGRADES, IT DOES NOT ONLY APPEND — and that is the whole
	// difference between it and the member merge above it.
	//
	// EVERY TRIP PARTICIPANT IS BY CONSTRUCTION A SHARE-HOLDER: the picker's
	// candidates ARE the car's accepted shares. So by the time this leg runs,
	// the share merge has ALREADY emitted a row for that car — projected
	// through the plain-VIEWER mask, which since MYR-602 carries no `location`.
	// A dedupe that merely skipped it (the member merge's rule) would leave the
	// participant holding a row with no coordinate for the entire window, and
	// the client's per-row pickup ETA (MYR-577) blank for the one car they were
	// invited to watch — the exact regression the member merge exists to
	// prevent for riders, reappearing one leg later for participants.
	//
	// So a NON-OWNER row for a car with an open window is REPLACED by the
	// trip_participant projection of the same catalog row. It is strictly
	// richer: the same fields plus the location group. `sharePermission`
	// survives because the row carries the caller's own AllowRides — the trip
	// elevates the ROLE, it does not replace the relationship.
	//
	// AN OWNER ROW IS NEVER TOUCHED. The owner mask is the widest there is, and
	// downgrading an owner to a participant on their own car would be a
	// narrowing dressed as a merge.
	index := make(map[string]int, len(resp.Items))
	for i, item := range resp.Items {
		if id, ok := item["vehicleId"].(string); ok {
			index[id] = i
		}
	}

	// One clock reading for this leg, matching the other three: every row of one
	// catalog response judges its MYR-491 setup state against the same instant.
	now := time.Now()
	for i := range rows {
		projected := nonOwnerSummaryMap(
			rows[i].VehicleCatalogRow,
			auth.ShareGrant{AllowRides: rows[i].AllowRides},
			now, auth.RoleTripParticipant,
		)

		at, existing := index[rows[i].ID]
		if !existing {
			index[rows[i].ID] = len(resp.Items)
			resp.Items = append(resp.Items, projected)
			continue
		}
		if wire, _ := resp.Items[at]["role"].(string); wire == string(auth.RoleOwner) {
			continue
		}
		resp.Items[at] = projected
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
