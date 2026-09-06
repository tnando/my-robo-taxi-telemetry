package telemetry

import (
	"net/http"
	"strconv"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// GET /api/trips/{tripId}/drives — §7.30.7.
//
// THE WINDOW'S DRIVES, in the §7.2 shape, for a caller who owns the trip or is
// a live participant of it. It is the FIRST surface on the platform that shows
// a non-owner a vehicle's drive history, and the bound is the whole safety
// argument: a trip participant sees the drives of the window they were part of
// and nothing else about the car's past.
//
// THE WINDOW IS RESOLVED FROM THE TRIP, never from the request. The handler
// passes a trip id and a user id; the store reads the window off the row it
// just authorized. A parameterised window would let somebody on trip A read
// trip B's drives by supplying B's dates.

// ServeDrives handles GET /api/trips/{tripId}/drives?limit=&cursor=.
func (h *TripHandler) ServeDrives(w http.ResponseWriter, r *http.Request) {
	tripID, ctx, userID, ok := h.beginTrip(w, r)
	if !ok {
		return
	}

	limit, cursor, ok := h.parseDrivesQuery(w, r)
	if !ok {
		return
	}

	page, err := h.trips.TripDrives(ctx, tripID, userID, cursor, limit)
	if err != nil {
		// ErrTripNotFound covers the stranger, the departed participant and the
		// unknown id alike — so a caller who is not on the trip cannot tell
		// this endpoint apart from one for a trip that never existed.
		h.failTrip(w, "drives", tripID, err)
		return
	}

	h.writeJSON(w, http.StatusOK, tripDrivesPage(page))
}

// parseDrivesQuery reuses §7.2's LIMIT BOUNDS AND CURSOR ENCODING verbatim.
//
// Same helper, same base64-JSON cursor, same [1, 100] range — so a client can
// page a trip's drives with exactly the code it pages a car's drives with, and
// a cursor is not silently a different thing on the two surfaces.
func (h *TripHandler) parseDrivesQuery(w http.ResponseWriter, r *http.Request) (int, DriveListCursor, bool) {
	limit := driveListDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < driveListMinLimit || n > driveListMaxLimit {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "limit must be an integer in [1, 100]")
			return 0, DriveListCursor{}, false
		}
		limit = n
	}

	var cursor DriveListCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeDriveCursor(raw)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed cursor")
			return 0, DriveListCursor{}, false
		}
		cursor = c
	}
	return limit, cursor, true
}

// tripDrivesPage builds the §7.2 envelope: `items`, `nextCursor`, `hasMore`.
//
// IT PROJECTS THROUGH THE `trip_participant` MASK even though that mask is
// currently the IDENTITY over DriveSummary — the role's field list is the same
// as the owner's, because what differs between them on this surface is which
// drives they may ask for, never what a drive says. Running the projection
// anyway is what makes a future narrowing land here for free instead of
// silently not applying; a hand-built map would be the one drives surface a
// mask change did not reach.
//
// The ROLE IS A CONSTANT, not resolved per request, and that is deliberate: a
// caller who reached this line has already been established by the store as
// owner-or-participant of THIS trip, and both see the same drive fields. Asking
// a role resolver again would add a query whose only possible answers are the
// two that produce identical output.
//
// The same `buildDriveSummary` and `encodeDriveCursor` §7.2 uses, so a client
// can page a trip's drives with exactly the code it pages a car's drives with.
func tripDrivesPage(page DriveListPage) drivesPageResponse {
	maskSpec := mask.For(mask.ResourceDriveSummary, auth.RoleTripParticipant)
	items := make([]map[string]any, 0, len(page.Items))
	for i := range page.Items {
		summary := buildDriveSummary(&page.Items[i])
		projected, _ := mask.Apply(summary.toMaskMap(), maskSpec)
		items = append(items, projected)
	}

	// nextCursor anchors on the LAST item and is null when there is no next
	// page — the same rule and the same encoding §7.2 uses.
	var nextCursor *string
	if page.HasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		encoded := encodeDriveCursor(DriveListCursor{StartTime: last.StartTime, ID: last.ID})
		nextCursor = &encoded
	}
	return drivesPageResponse{Items: items, NextCursor: nextCursor, HasMore: page.HasMore}
}
