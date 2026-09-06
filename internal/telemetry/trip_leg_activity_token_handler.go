package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// POST / DELETE /api/trips/{tripId}/legs/{legId}/activity-token — §7.30.10 and
// §7.30.11, the LEG anchor of the per-Activity path §7.21.1 and §7.21.2 define.
//
// THE OTHER HALF OF PUSH-TO-START, and without it the leg card is a one-way
// surface. §7.30.8 registers the token that lets the SERVER create a card;
// ActivityKit then hands the app a second, per-ACTIVITY token addressing that
// one running card, and this is where it is filed. With no route for it the
// server could raise a card and never update or end it: every leg's card would
// hold its opening state until ActivityKit's own ~8-hour ceiling removed it,
// still saying the car was driving somewhere it reached hours before.
//
// TWO TOKENS, TWO TABLES, TWO MEANINGS OF A 410 — see
// internal/store/trip_activity_token_repo.go. Nothing here touches the
// push-to-start registry.
//
// THE SHAPES ARE §7.21.1's AND §7.21.2's VERBATIM — same `activityToken` body
// key, same sandbox flag, same 256-character and hex validation, same
// `{registered, sandbox}` and `{ended}` responses, same 409 on a closed anchor.
// §7.21.7 says the device registers "through the EXISTING per-Activity path,
// extended to accept a leg anchor", and a leg-shaped variant of those bodies
// would make an installed client's ride and trip registration code two
// implementations of one thing.
//
// WHY THE ROUTE IS UNDER /api/trips AND NOT /api/ride-requests: the anchor is a
// leg, the authorization is trip membership, and the kill switch is
// TRIPS_ENABLED. Hanging it off the ride surface would put a trip's 503 behind
// a ride handler.

// ServeRegisterLegActivityToken handles POST — 200 `{registered, sandbox}`.
//
// UPSERT ON (leg, user), because ActivityKit rotates an Activity's update token
// mid-life and expects the server to switch to it. A rotation is an ordinary
// re-registration, and it clears any end tombstone: a client that re-registers
// is telling us it has a live card again.
func (h *TripHandler) ServeRegisterLegActivityToken(w http.ResponseWriter, r *http.Request) {
	tripID, legID, ctx, userID, ok := h.beginLeg(w, r)
	if !ok {
		return
	}

	var body activityTokenRequest
	if !h.decode(w, r, &body) {
		return
	}
	token := strings.TrimSpace(body.ActivityToken)
	switch {
	case token == "":
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "activityToken is required")
		return
	case len(token) > maxActivityTokenLen:
		// The token is P1: report the violation, never the value.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "activityToken is too long")
		return
	case !isHexToken(token):
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"activityToken must be hexadecimal")
		return
	}

	// THE MEMBERSHIP READ IS THE GATE, exactly as it is on §7.30.8. Registering
	// an update token is granting the server permission to write to this
	// phone's lock screen about this trip, so it has to be a trip the caller is
	// actually on — and GetTrip answers 404 for everybody else, so the endpoint
	// is not an oracle for trip ids either.
	if _, err := h.trips.GetTrip(ctx, tripID, userID); err != nil {
		h.failTrip(w, "register leg activity token", tripID, err)
		return
	}

	// THE LEG IS SCOPED TO THE TRIP INSIDE THE WRITE, not checked here: the
	// statement's own SELECT requires the leg to belong to this trip AND to
	// still be open, so a leg id from another trip is refused by the same
	// mechanism that refuses a closed one, under the same race. A check up here
	// would be a second, weaker copy of a rule the statement already holds.
	err := h.trips.RegisterTripLegActivityToken(ctx, tripID, legID, userID, token, body.Sandbox)
	switch {
	case err == nil:
	case errors.Is(err, ErrLiveActivityClosed):
		// The leg ended, or it is not this trip's. ONE ANSWER FOR BOTH, and
		// deliberately: distinguishing them would tell a caller whether a leg
		// id they guessed exists on somebody's trip. The instruction is the
		// same either way — end the Activity locally.
		h.logger.Info("trips: leg activity registration refused; the leg is closed or not on this trip",
			slog.String("trip_id", tripID),
			slog.String("leg_id", legID),
		)
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"this leg is no longer accepting Live Activity registrations")
		return
	default:
		h.failTrip(w, "register leg activity token", legID, err)
		return
	}

	// The response echoes NO TOKEN, for §7.21.1's reason: the value is P1 and
	// the caller already knows what it sent.
	h.writeJSON(w, http.StatusOK, activityTokenResponse{Registered: true, Sandbox: body.Sandbox})
}

// ServeEndLegActivityToken handles DELETE — 200 `{ended}`, idempotent.
//
// NO MEMBERSHIP READ, and the asymmetry with the POST is the same one §7.30.9
// makes. This only ever removes the CALLER'S OWN row, so the worst a stranger
// achieves is deleting a row they do not have; requiring a read would add a 404
// that tells them whether the trip is real, for a call that changes nothing
// either way. And a participant who has just LEFT the trip must still be able
// to clear their registration — after leaving they no longer pass the read.
//
// `false` covers both "already ended" and "never registered", deliberately
// indistinguishable: the client's own end and a server-side end race by design
// and both are correct.
func (h *TripHandler) ServeEndLegActivityToken(w http.ResponseWriter, r *http.Request) {
	_, legID, ctx, userID, ok := h.beginLeg(w, r)
	if !ok {
		return
	}
	ended, err := h.trips.EndTripLegActivityToken(ctx, legID, userID)
	if err != nil {
		h.failTrip(w, "end leg activity token", legID, err)
		return
	}
	h.writeJSON(w, http.StatusOK, activityEndedResponse{Ended: ended})
}

// beginLeg is beginTrip plus the leg id.
func (h *TripHandler) beginLeg(w http.ResponseWriter, r *http.Request) (
	tripID, legID string, ctx context.Context, userID string, ok bool,
) {
	legID = r.PathValue("legId")
	if legID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing legId")
		return "", "", nil, "", false
	}
	tripID, ctx, userID, ok = h.beginTrip(w, r)
	if !ok {
		return "", "", nil, "", false
	}
	return tripID, legID, ctx, userID, true
}
