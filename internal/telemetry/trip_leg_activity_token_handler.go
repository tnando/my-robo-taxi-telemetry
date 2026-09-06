package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// POST / DELETE /api/trip-legs/{legId}/activity-token — rest-api.md §7.21.7.
//
// THE OTHER HALF OF PUSH-TO-START, and without it the leg card is a one-way
// surface. §7.30.8 registers the token that lets the SERVER create a card;
// ActivityKit then hands the app a second, per-ACTIVITY token addressing that
// one running card, and this is where it is filed. With no route for it the
// server could raise a card and never update or end it: every leg's card would
// hold its opening state until ActivityKit's own ~8-hour ceiling removed it,
// still saying the car was driving somewhere it reached hours before.
//
// A DEDICATED ROUTE, NOT AN OVERLOADED RIDE ONE. The obvious alternative was to
// let §7.21.1's `/api/ride-requests/{id}/activity-token` accept a leg id in its
// path. It was rejected: that path's segment is typed by its name, the handler
// behind it resolves a RIDE and applies ride-shaped guards (terminal status, a
// lapsed reservation), and a caller passing the wrong kind of id would meet a
// 404 whose meaning depended on which table the server happened to look in
// first. Two anchors, two routes, one BODY — which is where the sharing
// actually belongs.
//
// THE PATH CARRIES NO TRIP ID, deliberately. A leg belongs to exactly one trip,
// so requiring the client to restate it would be asking it to prove something
// the server already knows — and a client that got the pair wrong would be
// refused for a reason that had nothing to do with its card. The authorization
// is resolved FROM the leg (store.TripLegAccess), in one statement.
//
// TWO TOKENS, TWO TABLES, TWO MEANINGS OF A 410 — see
// internal/store/trip_activity_token_repo.go. Nothing here touches the
// push-to-start registry.
//
// THE BODY AND THE RESPONSES ARE §7.21.1's AND §7.21.2's VERBATIM — the
// unchanged `RegisterLiveActivityRequest` {activityToken, sandbox}, the same
// 256-character and hex validation, the same `{registered, sandbox}` and
// `{ended}` shapes, the same 409 on a closed anchor. An installed client's ride
// and trip registration code is therefore ONE implementation with two URLs.
//
// THE KILL SWITCH APPLIES. These routes are part of trips, so TRIPS_ENABLED=false
// answers 503 here as it does on every §7.30 route — a feature that can be
// switched off has to be switched off whole.

// ServeRegisterLegActivityToken handles POST — 200 `{registered, sandbox}`.
//
// UPSERT ON (leg, user), because ActivityKit rotates an Activity's update token
// mid-life and expects the server to switch to it. A rotation is an ordinary
// re-registration, and it clears any end tombstone: a client that re-registers
// is telling us it has a live card again.
func (h *TripHandler) ServeRegisterLegActivityToken(w http.ResponseWriter, r *http.Request) {
	legID, ctx, userID, ok := h.beginLeg(w, r)
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

	// THE ACCESS PROBE IS THE GATE, and it answers the two refusals apart.
	// Registering an update token grants the server permission to write to this
	// phone's lock screen about this trip, so it has to be a trip the caller is
	// on — and an unknown leg is 404 identically to somebody else's, so the
	// endpoint is not an oracle for leg ids.
	tripID, open, err := h.trips.TripLegAccess(ctx, legID, userID)
	if err != nil {
		h.failTrip(w, "register leg activity token", legID, err)
		return
	}
	if !open {
		// A MEMBER, and their leg has ENDED. 409, not 404: they hold a real
		// card for a real leg of a real trip, and the instruction is to end it
		// locally — the same answer §7.21.1 gives a rider whose ride closed.
		h.logger.Info("trips: leg activity registration refused; the leg has ended",
			slog.String("trip_id", tripID),
			slog.String("leg_id", legID),
		)
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"this leg has ended and is no longer accepting Live Activity registrations")
		return
	}

	// The trip id comes from the PROBE, never from the caller. The write
	// re-asserts it alongside `ended_at IS NULL`, so a leg that closed between
	// the probe and the write is refused by the statement rather than by a
	// check that has already gone stale.
	err = h.trips.RegisterTripLegActivityToken(ctx, tripID, legID, userID, token, body.Sandbox)
	switch {
	case err == nil:
	case errors.Is(err, ErrLiveActivityClosed):
		// The leg closed between the probe and the write — the race the SQL
		// guard exists for. Same 409, same instruction.
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"this leg has ended and is no longer accepting Live Activity registrations")
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
// NO ACCESS PROBE, unlike the POST, and the asymmetry is the same one §7.30.9
// makes. This only ever removes the CALLER'S OWN row, so the worst a stranger
// achieves is deleting a row they do not have; requiring a probe would add a
// 404 that tells them whether the leg is real, for a call that changes nothing
// either way. And a participant who has just LEFT the trip must still be able
// to clear their registration — after leaving they no longer pass the probe.
//
// `false` covers both "already ended" and "never registered", deliberately
// indistinguishable: the client's own end and a server-side end race by design
// and both are correct.
func (h *TripHandler) ServeEndLegActivityToken(w http.ResponseWriter, r *http.Request) {
	legID, ctx, userID, ok := h.beginLeg(w, r)
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

// beginLeg runs the preamble both routes share: the kill switch, the bearer
// token, and the leg id. There is no trip id on this path — see the file
// header.
func (h *TripHandler) beginLeg(w http.ResponseWriter, r *http.Request) (
	legID string, ctx context.Context, userID string, ok bool,
) {
	legID = r.PathValue("legId")
	if legID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing legId")
		return "", nil, "", false
	}
	ctx, userID, ok = h.begin(w, r)
	if !ok {
		return "", nil, "", false
	}
	return legID, ctx, userID, true
}
