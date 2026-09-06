package telemetry

import (
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// POST /api/vehicles/{vehicleId}/trips — §7.30.1, and the ONE route on this
// surface that behaves differently from the other ten.
//
// Split from trip_handler.go so both stay inside the 300-line cap, and it is
// the right thing to lift out on its own: create is the only route keyed on a
// VEHICLE rather than on a trip, the only one that answers 403 rather than 404,
// and the only one that may fire two pushes for one request. Every one of those
// is an exception to a rule the file next door states, and an exception reads
// better beside its own reasoning than buried among the routes that follow it.

// ServeCreate handles POST /api/vehicles/{vehicleId}/trips — OWNER ONLY.
func (h *TripHandler) ServeCreate(w http.ResponseWriter, r *http.Request) {
	vehicleID := r.PathValue("vehicleId")
	if vehicleID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId")
		return
	}
	ctx, userID, ok := h.begin(w, r)
	if !ok {
		return
	}

	var body createTripBody
	if !h.decode(w, r, &body) {
		return
	}
	in, ok := h.parseCreate(w, vehicleID, userID, body)
	if !ok {
		return
	}

	// OWNERSHIP IS RESOLVED AGAINST THE VEHICLE ROW, not against the trip —
	// there is no trip yet. This is the ONE place on §7.30 that answers 403:
	// the caller named a vehicle, and a caller who can name a vehicle already
	// knows it exists (they read it out of their catalog), so there is nothing
	// left for a 404 to conceal.
	if !h.verifyVehicleOwner(ctx, w, vehicleID, userID) {
		return
	}

	trip, err := h.trips.CreateTrip(ctx, in)
	if err != nil {
		h.failTrip(w, "create", vehicleID, err)
		return
	}

	// PUSHES AFTER THE COMMIT, never inside it. A notification about a trip
	// that then failed to save is the one failure mode worse than a trip
	// nobody was told about.
	recipients := participantUserIDs(trip)
	h.notifier.TripAdded(ctx, trip, recipients)
	if trip.Role == tripRoleOwner && tripStatusOf(trip, time.Now()) == tripStatusActive {
		// A window that is ALREADY OPEN at creation — the common case for a
		// road trip already underway — starts immediately, so the `trip_started`
		// that the sweeper would otherwise send at the boundary is owed right
		// now. The sweeper's `started_notified_at` stamp is what stops the two
		// of them sending it twice.
		h.notifier.TripStarted(ctx, trip, recipients)
	}

	h.writeJSON(w, http.StatusCreated, tripWire(trip, userID))
}
