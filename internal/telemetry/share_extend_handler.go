package telemetry

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// POST /api/vehicles/{vehicleId}/share/extend — the owner adding somebody who
// ALREADY sees one of their cars to another one, in one call (MYR-609,
// rest-api.md §7.5.8).
//
// It is the fifth owner route on this handler and the only one that produces an
// ACCEPTED row without anybody redeeming anything, so it is worth stating what
// makes that legitimate rather than leaving it implied:
//
//   - The grantee already accepted a share FROM THIS OWNER. The relationship
//     exists; the endpoint adds a car to it.
//   - Both halves are the CALLER's — the source grant (`owner_user_id`) and the
//     target car — checked in SQL on both, so this can never reach across
//     owners.
//   - The owner could already do this unilaterally: §7.5.1 mints one code
//     across N of their vehicles. What is removed is the second redemption, not
//     the grantee's say.
//
// EVERY REFUSAL IS ONE OF THE FOUR §7.5 ANSWERS, unchanged. 401 for no token,
// 404 for an unknown path vehicle, 403 `vehicle_not_owned` for somebody else's
// car, and 404 `not_found` for a source grant that is missing, foreign, pending
// or revoked — the four INDISTINGUISHABLE, because an endpoint that told them
// apart would be an oracle for other owners' invite ids.

// extendShareRequest is the POST body: `{"shareId":"…"}` and nothing else.
//
// STRICTLY DECODED (DisallowUnknownFields below), unlike the PATCH body one
// surface over. The asymmetry is deliberate: a patch carries OPTIONAL fields
// where a typo'd key means "leave it alone" and a strict decoder would break a
// client sending a field a later version adds, whereas this body has exactly
// one REQUIRED field and a typo'd key means the request names no share at all.
// Failing loudly is the only reading that cannot look like success.
type extendShareRequest struct {
	ShareID string `json:"shareId"`
}

// ServeExtend handles POST /api/vehicles/{vehicleId}/share/extend.
//
// 201 with the §7.5 ShareInvite row for the NEW grant, masked for the owner
// exactly as create's is. Errors:
//   - 400 invalid_request — malformed or unknown-keyed body, or a blank shareId.
//   - 401 auth_failed — missing/invalid token.
//   - 403 vehicle_not_owned — the caller does not own the path vehicle.
//   - 404 not_found — the path vehicle does not exist, or the source share is
//     missing / another owner's / not accepted, indistinguishably.
//   - 409 conflict + subCode already_shared — that person already holds a live
//     grant on this car. Also what extending a grant onto its OWN vehicle
//     answers, because that IS the already-shared case.
//   - 409 conflict, NO subCode — the source grant is paused, the grantee's
//     grant on this car is paused, or the grantee LEFT this car. Three states
//     the owner must resolve elsewhere, each with its own message; see
//     writeExtendError for why none of them borrows already_shared.
//   - 409 invalid_request — the MYR-599 driver-access car is still awaiting its
//     owner-approval acknowledgment (the same gate ServeCreate carries).
func (h *ShareInviteHandler) ServeExtend(w http.ResponseWriter, r *http.Request) {
	vehicle, vehicleID, userID, ok := h.authOwner(w, r, "share extend")
	if !ok {
		return
	}
	// THE SAME CONSENT GATE AS CREATE, and for the identical reason: extending
	// a grant is an act of DISPOSAL over somebody else's property — it hands a
	// third party standing access to a car whose owner has not yet been
	// recorded as agreeing it belongs here. A route that skipped this would be
	// a way around §7.29 that the create path closes.
	if !h.driverAccessAllows(w, vehicle, vehicleID, userID, "share extend") {
		return
	}

	shareID, ok := h.decodeExtend(w, r)
	if !ok {
		return
	}

	row, granteeID, err := h.invites.ExtendShare(r.Context(), ShareExtendInput{
		OwnerUserID:     userID,
		TargetVehicleID: vehicleID,
		SourceShareID:   shareID,
	})
	if err != nil {
		h.writeExtendError(w, vehicleID, shareID, err)
		return
	}

	// BUST THE GRANTEE'S CACHE, NOT THE OWNER'S — the MYR-184 bust-on-mutation
	// pattern, in the WIDENING direction for once. The cached access set is
	// what the WebSocket handshake and every per-vehicle handler consult, so
	// without this the new car is invisible to the very person it was just
	// shared with until their entry lapses (5-minute TTL) — and the owner's app
	// would be showing them as a pickable participant on a car their own client
	// cannot yet resolve.
	if h.access != nil && granteeID != "" {
		h.access.InvalidateVehicles(granteeID)
	}

	// THEN WIDEN THE SOCKET THEY ALREADY HOLD, and the order is the same
	// correctness argument the narrowing paths make: the widen provokes a
	// reconnect, and a handshake served from a stale access set would come back
	// WITHOUT the car — turning the fix into a no-op that looks like one that
	// worked.
	//
	// An earlier cut asserted there was deliberately no socket signal here,
	// reasoning that ShareAccessNotifier exists to END a stream and gaining
	// access opens nothing that needs closing. The second half is true and the
	// conclusion does not follow. `Client.vehicleIDs` is frozen at handshake,
	// so a grantee who is CONNECTED when the owner extends does not get the car
	// at all until they happen to reconnect — up to a whole session — while
	// their own REST surface already shows it. The owner is told the share
	// worked, the grantee's map does not have the car, and neither of them can
	// see why. `next handshake reads the widened set` was the mechanism; what
	// was missing was anything that causes a next handshake.
	h.widenLiveAccess(granteeID, vehicleID, "extended")

	// Ids only: the new grant, the source it was copied from, the car and both
	// parties. Never the label (P1) and never the code — which on an accepted
	// row is blanked in SQL before it could reach here anyway.
	h.logger.Info("share extended",
		slog.String("invite_id", row.ID),
		slog.String("source_invite_id", shareID),
		slog.String("vehicle_id", vehicleID),
		slog.String("owner_user_id", userID),
		slog.String("user_id", granteeID),
		slog.Bool("allow_rides", row.Grant.AllowRides),
		slog.Bool("suspended", row.Grant.Suspended),
	)

	// NO LINK CONTEXT, AND NO LOOKUP TO BUILD ONE — the same argument
	// ServePatch makes. `shareUrl` is minted inside toShareInviteWire's PENDING
	// branch, beside `code`, because a share link is a wrapper around the
	// credential. This row is born ACCEPTED, so that branch cannot execute, and
	// passing h.linkCtx here would buy an OwnerFirstName query per call whose
	// only destination is unreachable — plus, on failure, a warning about a
	// degraded share link that was never going to be minted.
	h.writeJSON(w, http.StatusCreated,
		toShareInviteMasked(&row, auth.RoleOwner, inviteLinkCtx{}))
}

// decodeExtend reads the body and rejects one that names no share.
//
// A blank or whitespace-only `shareId` is 400, NOT 404. The 404 body on this
// endpoint means "that share is not extendable by you", and it is deliberately
// indistinguishable across four different causes; blurring a client bug into
// that set would make the indistinguishability harder to reason about rather
// than easier — the same split ServeJoin draws between a malformed code (400)
// and a dead one (404).
func (h *ShareInviteHandler) decodeExtend(w http.ResponseWriter, r *http.Request) (string, bool) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body extendShareRequest
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return "", false
	}
	shareID := strings.TrimSpace(body.ShareID)
	if shareID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "shareId is required")
		return "", false
	}
	return shareID, true
}

// writeExtendError maps an extend failure onto a response.
//
// FOUR DISTINCT 409s, and only ONE of them carries a sub-code. `already_shared`
// means the call is a SUCCESS to render — the person has the car — which is
// what makes an "Add all" affordance safe. The other three mean nothing
// happened and name a different thing the owner must do first, on a different
// screen; a client cannot act on those generically, so they are told in the
// message and the sub-code stays absent rather than being stretched to cover
// refusals it would misdescribe.
//
// ErrShareVehicleNotOwned is 403 here and not 404, and it emits
// `vehicle_not_owned` rather than create's `permission_denied`. The code is the
// vehicle-scoped specialization of the same 403 and it is the right one here:
// the request names a vehicle in the PATH, and the caller has already proved
// they own it to reach this code — there is no existence left to protect, so
// the more specific code costs nothing and tells a client which resource it was
// about. (An earlier comment claimed parity with create's mapping. There was
// none; create emits `permission_denied` over a whole `vehicleIds` set.) In
// practice the store's target-ownership check cannot fire behind authOwner — it
// is the second half of a belt-and-braces pair — but the status it would
// produce is spelled out rather than falling into the 500 default.
func (h *ShareInviteHandler) writeExtendError(w http.ResponseWriter, vehicleID, shareID string, err error) {
	switch {
	case errors.Is(err, ErrShareAlreadyGranted):
		// The ONE refusal that names a fact about the caller's own data, so
		// it says something useful. The sub-code is what a client branches
		// on to skip that person in the picker rather than retrying.
		wserrors.WriteErrorEnvelopeSub(w, h.logger, http.StatusConflict,
			wserrors.ErrCodeConflict, wserrors.SubCodeAlreadyShared,
			"that person already has access to this car")
	case errors.Is(err, ErrShareSourceSuspended):
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"that share is paused — restore it in Share first")
	case errors.Is(err, ErrShareTargetSuspended):
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"that person is paused on this car — restore them in Share")
	case errors.Is(err, ErrShareGranteeLeft):
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"they left this car — send them a new invite")
	case errors.Is(err, sdk.ErrNotFound):
		// Missing, foreign, pending, revoked — one body for all four.
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "share not found")
	case errors.Is(err, ErrShareVehicleNotOwned):
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
	default:
		h.logger.Error("share extend failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("source_invite_id", shareID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
	}
}
