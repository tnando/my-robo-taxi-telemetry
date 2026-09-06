package telemetry

import (
	"context"
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The PER-TRIP routes of §7.30: read, patch, end, leave.
//
// ALL FOUR ANSWER 404 TO A CALLER WHO IS NOT ON THE TRIP, identically to how
// they answer for a trip that does not exist. The store makes that structural —
// its reads resolve the caller's role in the same statement that reads the row
// and return ErrTripNotFound when the role is NULL — so no handler here has a
// separate "are they allowed" branch that could answer differently.

// ServeGet handles GET /api/trips/{tripId} — owner or live participant.
func (h *TripHandler) ServeGet(w http.ResponseWriter, r *http.Request) {
	tripID, ctx, userID, ok := h.beginTrip(w, r)
	if !ok {
		return
	}

	trip, err := h.trips.GetTrip(ctx, tripID, userID)
	if err != nil {
		h.failTrip(w, "get", tripID, err)
		return
	}
	h.writeJSON(w, http.StatusOK, tripWire(trip, userID))
}

// ServePatch handles PATCH /api/trips/{tripId} — OWNER ONLY, 404 to everybody
// else including a participant.
//
// A participant getting 404 rather than 403 is deliberate and is the one place
// the rule costs something in clarity: they can SEE the trip through GET, so a
// 404 from PATCH reads as odd. It is still the right answer — the alternative
// leaks nothing here but would make the surface's rule conditional, and a rule
// with one exception is a rule somebody applies inconsistently next time.
func (h *TripHandler) ServePatch(w http.ResponseWriter, r *http.Request) {
	tripID, ctx, userID, ok := h.beginTrip(w, r)
	if !ok {
		return
	}

	var body updateTripBody
	if !h.decode(w, r, &body) {
		return
	}
	in, ok := h.parseUpdate(w, body)
	if !ok {
		return
	}

	// READ BEFORE WRITE, purely so the push fan-out can name the people this
	// patch ACTUALLY ADDED. Sending `trip_added` to the whole roster on every
	// patch would re-notify everybody already on the trip, which reads as the
	// trip having been created a second time. A failure here is the caller's
	// answer (404 for a non-owner), so the read is a gate as well as a
	// baseline.
	before, err := h.trips.GetTrip(ctx, tripID, userID)
	if err != nil {
		h.failTrip(w, "patch", tripID, err)
		return
	}

	trip, err := h.trips.UpdateTrip(ctx, tripID, userID, in)
	if err != nil {
		h.failTrip(w, "patch", tripID, err)
		return
	}

	if added := newParticipantUserIDs(before, trip); len(added) > 0 {
		h.notifier.TripAdded(ctx, trip, added)
	}
	// REMOVAL SENDS NOTHING, and that is a decision rather than an omission.
	// The contract lists five `trips` events and none of them is "you were
	// removed"; the person's live access ends the moment the row is stamped
	// (the WS revalidator drops them, as it does for a revoked share), and
	// announcing it would be telling somebody they have been taken off a trip
	// by a person who chose not to tell them.

	h.writeJSON(w, http.StatusOK, tripWire(trip, userID))
}

// ServeEnd handles POST /api/trips/{tripId}/end — OWNER ONLY, IDEMPOTENT 200.
//
// 200 rather than 204 because the response carries the trip, which is what
// tells the client the new `endedAt` and the now-`ended` status without a
// second call. Idempotent all the way down: the store's statement is guarded on
// `ended_at IS NULL`, so a double-tap re-reads and returns the same trip rather
// than moving the end forward by however long the two taps were apart.
func (h *TripHandler) ServeEnd(w http.ResponseWriter, r *http.Request) {
	tripID, ctx, userID, ok := h.beginTrip(w, r)
	if !ok {
		return
	}

	// The BEFORE state decides whether a push is owed. Ending an already-ended
	// trip must not re-announce it: the first call already told everybody, and
	// a second announcement about the same fact is how a notification category
	// gets turned off.
	before, err := h.trips.GetTrip(ctx, tripID, userID)
	if err != nil {
		h.failTrip(w, "end", tripID, err)
		return
	}
	wasLive := tripStatusOf(before, time.Now()) != tripStatusEnded

	trip, err := h.trips.EndTrip(ctx, tripID, userID)
	if err != nil {
		h.failTrip(w, "end", tripID, err)
		return
	}
	if wasLive {
		h.notifier.TripEnded(ctx, trip, participantUserIDs(before))
	}

	h.writeJSON(w, http.StatusOK, tripWire(trip, userID))
}

// ServeLeave handles DELETE /api/trips/{tripId}/participants/me — 204, always.
//
// SILENT AND IDEMPOTENT BY DESIGN. It reports nothing about whether the trip
// exists, whether the caller was ever on it, or whether they had already left,
// and it answers 204 in every one of those cases. A 404 for "not a member"
// would tell any authenticated caller which trip ids are real, which is exactly
// what the rest of this surface refuses to do — and there is nothing a client
// would do differently with the distinction: it wanted to not be on the trip,
// and it is not on the trip.
//
// The OWNER leaving is a no-op that also answers 204. An owner holds no
// participant row, so there is nothing to stamp; ending the trip is the
// operation they want, and it has its own route.
func (h *TripHandler) ServeLeave(w http.ResponseWriter, r *http.Request) {
	tripID, ctx, userID, ok := h.beginTrip(w, r)
	if !ok {
		return
	}

	if err := h.trips.LeaveTrip(ctx, tripID, userID); err != nil {
		// A genuine transport failure IS reported — 204 is the answer to "you
		// are not on this trip", not to "the database is down". Reporting 204
		// here would tell the client they had left when they had not, and they
		// would keep receiving the car's location.
		h.failTrip(w, "leave", tripID, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// beginTrip is `begin` plus the {tripId} path value every per-trip route needs.
func (h *TripHandler) beginTrip(w http.ResponseWriter, r *http.Request) (tripID string, ctx context.Context, userID string, ok bool) {
	tripID = r.PathValue("tripId")
	if tripID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing tripId")
		return "", nil, "", false
	}
	ctx, userID, ok = h.begin(w, r)
	if !ok {
		return "", nil, "", false
	}
	return tripID, ctx, userID, true
}
