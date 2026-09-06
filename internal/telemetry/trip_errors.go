package telemetry

import (
	"errors"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// THE ERROR MAP for the MYR-602 trips surface (rest-api.md §7.30).
//
// EVERY REFUSAL RIDES AN EXISTING TOP-LEVEL CODE. `wserrors.ErrorCode` is a
// CLOSED enum the SDKs decode into a generated union, so a new member is a
// five-file change across two repos and a breaking decode on every shipped
// client that has not been rebuilt. Trips needed no new one: the refusals are
// an illegal state (`conflict`), a bad body (`invalid_request`), a denial
// (`vehicle_not_owned`), an absence (`not_found`) and a disabled feature
// (`service_unavailable`) — five things the vocabulary already says.
//
// WHAT IS NEW IS THREE SUB-CODES, which is the mechanism that exists exactly
// for this: telling a client WHICH conflict, when the primary code alone does
// not say what to do next. `subCode` is an open string on
// ws-messages.schema.json — the enum is documented in prose, not declared — so
// emitting a new value is schema-VALID today and needs no client rebuild.
//
// ⚠ CONTRACT NOTE: contracts v0.41.0 does NOT yet list these three in that
// prose enum. The values are chosen to match the spec's own wording
// (`trip_overlaps`, `participant_not_shared`) so the upstream addition is a
// documentation change rather than a rename. Recorded as a divergence in
// rest-api.md §10.

const (
	// SubCodeTripOverlaps qualifies `conflict` when another scheduled-or-active
	// trip on the same vehicle already covers part of the requested window.
	//
	// It earns a sub-code because the client's next move is specific and not
	// derivable from `conflict` alone: send the owner back to the DATE PICKER
	// with the knowledge that the car — not the trip, not the participants —
	// is the thing that is double-booked. The same reasoning that gave
	// `time_conflict` its own sub-code on §7.8, one surface over.
	SubCodeTripOverlaps wserrors.SubCode = "trip_overlaps"

	// SubCodeParticipantNotShared qualifies `invalid_request` when one of the
	// requested participants is not a live accepted share-holder on this car.
	//
	// DELIBERATELY UNSPECIFIC ABOUT WHICH ONE, and about why. "No such share",
	// "a share on a different car", "an invite never redeemed" and "a
	// suspended grant" are one answer, because naming the failing id would
	// make the endpoint an oracle for other people's share ids. The client's
	// action is the same in all four cases: re-fetch the roster and let the
	// owner pick again.
	SubCodeParticipantNotShared wserrors.SubCode = "participant_not_shared"

	// SubCodeTripEnded qualifies `conflict` on a mutation of a trip whose
	// window has already closed.
	//
	// It exists because a bare `conflict` on a trip the owner is looking at
	// right now is indistinguishable from a server bug, and the client would
	// retry. With this it can say the true thing — the trip is over, start a
	// new one — which is also the only thing that would work: extending
	// `endsAt` past NOW() on a lapsed trip would resurrect live access every
	// participant was already told had ended.
	SubCodeTripEnded wserrors.SubCode = "trip_ended"
)

// tripStoreErrors is the consumer-site view of the store's sentinels. Declared
// here rather than imported, because internal/telemetry never imports
// internal/store (the dependency rule) — the adapter in cmd/ translates.
//
// They are compared with errors.Is through the ADAPTER'S wrapping, so a store
// error that travels through two layers still lands on the right status.
var (
	// ErrTripNotFound is an unknown trip id AND a trip the caller has no
	// relationship to. ONE sentinel for both, deliberately: a trip somebody
	// else owns must be indistinguishable from a trip that does not exist, or
	// the endpoint is an oracle for trip ids.
	ErrTripNotFound = errors.New("trip not found")

	// ErrTripOverlaps → 409 conflict / trip_overlaps.
	ErrTripOverlaps = errors.New("trip window overlaps an existing trip")

	// ErrTripParticipantNotShared → 400 invalid_request / participant_not_shared.
	ErrTripParticipantNotShared = errors.New("participant holds no accepted share on this vehicle")

	// ErrTripWindowInvalid → 400 invalid_request (no sub-code: the message
	// names the rule, and there is nothing for a client to branch on).
	ErrTripWindowInvalid = errors.New("invalid trip window")

	// ErrTripNameInvalid → 400 invalid_request.
	ErrTripNameInvalid = errors.New("invalid trip name")

	// ErrTripEnded → 409 conflict / trip_ended.
	ErrTripEnded = errors.New("trip has already ended")
)

// writeTripError maps a store-layer error onto the wire.
//
// ONE MAPPING FOR EVERY TRIP ENDPOINT, so the same condition cannot answer 409
// on one route and 400 on another — which is the drift that makes a client's
// error handling a per-endpoint special case.
//
// The default arm is 500 and it logs. An unrecognised error is a bug, not a
// client mistake, and reporting it as a 4xx would tell the caller to change
// their request when nothing they can do would help.
//
// Returns false when the error was not a known trip refusal, so the caller can
// add its own context to the log before falling through.
func (h *TripHandler) writeTripError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, ErrTripNotFound):
		// 404 EVERYWHERE ON THIS SURFACE, including for a trip the caller
		// simply is not on. See the sentinel's own comment.
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "trip not found")
	case errors.Is(err, ErrTripOverlaps):
		h.writeErrorSub(w, http.StatusConflict, wserrors.ErrCodeConflict, SubCodeTripOverlaps,
			"this vehicle already has a trip covering part of that window")
	case errors.Is(err, ErrTripEnded):
		h.writeErrorSub(w, http.StatusConflict, wserrors.ErrCodeConflict, SubCodeTripEnded,
			"this trip has ended and can no longer be changed")
	case errors.Is(err, ErrTripParticipantNotShared):
		h.writeErrorSub(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, SubCodeParticipantNotShared,
			"every participant must be someone this vehicle is already shared with")
	case errors.Is(err, ErrTripWindowInvalid):
		// ALL THREE WINDOW RULES IN ONE SENTENCE, rather than the underlying
		// error's text. The store's message is wrapped ("store: …") and
		// wrapping is not a wire contract; more importantly a message is not
		// something a client may branch on (§4.1 rule 1), so its only job is
		// to be readable by the person who typed the dates. It carries no user
		// content — the name is P1 and never appears in an error.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"endsAt must be after startsAt, no more than 30 days later, and not in the past")
	case errors.Is(err, ErrTripNameInvalid):
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"name must be 1 to 60 characters after trimming, with no control characters")
	default:
		return false
	}
	return true
}
