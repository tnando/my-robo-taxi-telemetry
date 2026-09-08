package telemetry

import (
	"context"
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// POST / DELETE /api/trips/{tripId}/activity-start-token — §7.30.8.
//
// A TRIP'S LIVE ACTIVITY IS PUSH-TO-START, which is the whole reason this
// endpoint exists and is shaped differently from §7.21's per-ride registration.
// A leg begins when the CAR sets off — an event the phone learns about from a
// push, while the app is very likely not running — and ActivityKit does not let
// a background app start an Activity. So the app registers a PUSH-TO-START
// token up front, once per trip, and the server starts the Activity when the
// leg begins. §7.21's token addresses one already-started Activity; this one
// addresses the CAPABILITY to start them.
//
// THE OWNER MAY REGISTER TOO, by explicit product decision: the owner is
// included in the per-leg Activity. So this route is open to anybody the store
// admits to the trip, and the store's read is the gate.
//
// THE TOKEN IS P1 AND A CAPABILITY. Whoever holds it together with the team's
// APNs signing key can start a Live Activity on that phone. It is never logged
// (not even at a prefix, on this path — nothing here has a reason to), never
// echoed in a response, and never placed in an error message. The 204 responses
// below carry no body at all, which is the strongest form of "the caller
// already knows what they sent".

// registerTripActivityBody is RegisterTripActivityRequest (trip.schema.json).
type registerTripActivityBody struct {
	PushToStartToken string `json:"pushToStartToken"`
	// Sandbox names the APNs gateway the token belongs to. A development or
	// TestFlight build mints a sandbox token and pushing it to production is
	// rejected as BadDeviceToken, so it is carried PER REGISTRATION rather
	// than read from the device registry — starting an Activity needs no
	// notification permission, so the user may have no device row at all.
	// Absent means production, matching §7.21.
	Sandbox bool `json:"sandbox"`
}

// tripActivityTokenMaxLen mirrors the schema's 256-character ceiling. Checked
// here so an oversized value is refused before it reaches a TEXT column and
// before anything tries to send it.
const tripActivityTokenMaxLen = 256

// tripActivityCatchUpTimeout bounds the catch-up push, which runs AFTER the
// response on a context detached from the request's. Detached means nothing
// else would ever stop it, so it needs a deadline of its own; generous, because
// it covers one claim statement plus one APNs round trip and its only cost on
// expiry is a card that the leg's next update raises anyway.
const tripActivityCatchUpTimeout = 15 * time.Second

// ServeRegisterActivityToken handles POST — 204, upsert.
//
// UPSERT, NOT INSERT, because ActivityKit ROTATES the push-to-start token. A
// re-registration replaces the value in place; accumulating rows would mean two
// starts on one phone for one leg, which is two Live Activities for the same
// journey on the same lock screen.
func (h *TripHandler) ServeRegisterActivityToken(w http.ResponseWriter, r *http.Request) {
	tripID, ctx, userID, ok := h.beginTrip(w, r)
	if !ok {
		return
	}

	var body registerTripActivityBody
	if !h.decode(w, r, &body) {
		return
	}
	if body.PushToStartToken == "" || len(body.PushToStartToken) > tripActivityTokenMaxLen {
		// The refusal names the FIELD and not the VALUE. An error message is
		// the one place a P1 value most reliably reaches a log without
		// anybody deciding it should.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"pushToStartToken must be 1 to 256 characters")
		return
	}

	// THE MEMBERSHIP READ IS THE GATE. Registering a token is granting the
	// server permission to write to this phone's lock screen about this trip,
	// so it has to be a trip the caller is actually on — and the store's read
	// answers 404 for everybody else, so the endpoint is not an oracle for
	// trip ids either.
	if _, err := h.trips.GetTrip(ctx, tripID, userID); err != nil {
		h.failTrip(w, "register activity token", tripID, err)
		return
	}

	if err := h.trips.RegisterTripActivityStartToken(ctx, tripID, userID, body.PushToStartToken, body.Sandbox); err != nil {
		h.failTrip(w, "register activity token", tripID, err)
		return
	}
	// NO BODY, deliberately: the response carries no token, because the value
	// is P1 and the caller already knows what it sent. Echoing it would only
	// put it in every client log and proxy trace — the same reasoning §7.21's
	// response gives for carrying no token.
	w.WriteHeader(http.StatusNoContent)
	// Flushed so the catch-up below cannot hold the 204 behind an APNs round
	// trip. A ResponseWriter that cannot flush is not an error — the answer is
	// then delivered when this handler returns, exactly as it was before.
	_ = http.NewResponseController(w).Flush()

	// THE CATCH-UP (MYR-612). A leg that is ALREADY under way gets its card
	// raised on THIS phone now, because the leg-open fan-out ran before this
	// row existed — which is the normal order of events, since registering is
	// what a phone does on receiving the leg-start push. Without this, a token
	// that lands three seconds late means no card for the whole leg, and on
	// 2026-09-08 that was every card on the trip.
	//
	// ⚠ AFTER THE 204, AND ON A CONTEXT THE CLIENT CANNOT CANCEL (MYR-612
	// review). It used to run on the request's own context before the response,
	// which made a background app's abandoned POST — a phone that registers as
	// it is being suspended, which is the exact circumstance this catch-up
	// exists for — cancel the very push it had just asked for. Worse, the send
	// is CLAIM-BEFORE-SEND: a cancellation landing between the claim and the
	// APNs write stamped `started_leg_id` for a card that was never raised, and
	// no other sender would try that device again for the rest of the leg.
	// Detached, with its own deadline, it is the same shape
	// releasePushToStartClaim already uses for the same reason.
	catchUpCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), tripActivityCatchUpTimeout)
	defer cancel()
	h.notifier.ActivityTokenRegistered(catchUpCtx, tripID, userID)
}

// ServeDeleteActivityToken handles DELETE — 204, idempotent.
//
// NO MEMBERSHIP READ, unlike the POST, and the asymmetry is deliberate. This
// operation only ever REMOVES the caller's own row, so the worst a stranger can
// achieve is deleting a row they do not have; requiring a read would add a
// 404 that tells them whether the trip is real, for a call that changes nothing
// either way. A participant who has just left must be able to clear their token
// — and after leaving they no longer pass the membership read.
func (h *TripHandler) ServeDeleteActivityToken(w http.ResponseWriter, r *http.Request) {
	tripID, ctx, userID, ok := h.beginTrip(w, r)
	if !ok {
		return
	}
	if err := h.trips.DeleteTripActivityStartToken(ctx, tripID, userID); err != nil {
		h.failTrip(w, "delete activity token", tripID, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
