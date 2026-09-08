package telemetry

import (
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// THE REQUEST BODIES §7.30 ACCEPTS, and their validation — the opposite
// direction from trip_wire.go, which holds everything this surface SENDS.
//
// Split from it because the two are genuinely separate concerns and the
// combined file passed the 300-line cap: a projection answers "what does a
// client read", these answer "what may a client say", and only the second half
// is where a strict decode, a required field and a 400 live.
//
// ⚠ THE DECODE IS STRICT ON BOTH SHAPES (unknown fields are a 400), and the
// optional members are POINTERS rather than zero values. That is what lets
// §7.30.4 tell an ABSENT key from a present-but-empty one, which the MYR-618
// participant branch needs: `{"removeParticipantIds": []}` is a participant
// reaching for an owner's verb and is refused, while omitting the key is not.

// createTripBody is CreateTripRequest (trip.schema.json). Every field is
// REQUIRED on the contract except `participantIds`, which may be empty — an
// owner may open a window and add people later.
//
// The instants are decoded as STRINGS and parsed explicitly rather than into
// time.Time, so a malformed date is a 400 naming the field instead of the
// decoder's own message about the whole body.
type createTripBody struct {
	// VehicleID is REQUIRED by the schema and is NOT the authority — the path
	// is. It is accepted here because the decode is STRICT: omitting the field
	// from this struct would make every conformant request a 400 for carrying
	// a field the contract says it must carry.
	//
	// A MISMATCH IS REFUSED rather than ignored. Silently preferring the path
	// would mean a client that got its own body wrong creates a trip on a
	// different car than it asked for and finds out from a support ticket; the
	// two values disagreeing is a client bug, and saying so is the useful
	// answer. Ownership is checked against the PATH either way, so the
	// mismatch can never be an escalation — only a confusion.
	VehicleID      string   `json:"vehicleId"`
	Name           string   `json:"name"`
	StartsAt       string   `json:"startsAt"`
	EndsAt         string   `json:"endsAt"`
	ParticipantIDs []string `json:"participantIds"`
}

// parseCreate validates and converts the body.
//
// THE PATH IS THE AUTHORITY for which car this is about. CreateTripRequest
// also carries `vehicleId` — for the SDKs' single-shape typing — and it is
// accepted, checked for agreement, and then discarded. A request whose body
// named a different car has two answers to a question with one, and the useful
// response is to say so rather than to quietly pick one.
func (h *TripHandler) parseCreate(w http.ResponseWriter, vehicleID, userID string, body createTripBody) (TripCreateInput, bool) {
	if body.VehicleID != "" && body.VehicleID != vehicleID {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"vehicleId in the body must match the one in the path")
		return TripCreateInput{}, false
	}
	startsAt, ok := h.parseInstant(w, "startsAt", body.StartsAt)
	if !ok {
		return TripCreateInput{}, false
	}
	endsAt, ok := h.parseInstant(w, "endsAt", body.EndsAt)
	if !ok {
		return TripCreateInput{}, false
	}
	return TripCreateInput{
		VehicleID:   vehicleID,
		OwnerUserID: userID,
		// NOT VALIDATED HERE. The store owns the name rules (trimmed, 1..60
		// RUNES, no control characters) and applies them to the create and the
		// patch through one function; a copy here would be a second rule to
		// drift.
		Name:                body.Name,
		StartsAt:            startsAt,
		EndsAt:              endsAt,
		ParticipantShareIDs: body.ParticipantIDs,
	}, true
}

// updateTripBody is UpdateTripRequest. Pointers so an ABSENT key ("leave this
// alone") is distinguishable from a present one — the distinction PATCH is
// entirely built on.
//
// ⚠ THE TWO LISTS ARE POINTERS TOO, SINCE MYR-618, and that is not symmetry for
// its own sake. A participant's PATCH is admitted for `addParticipantIds` and
// refused outright for anything else, so the handler has to distinguish
// `{"removeParticipantIds": []}` — a present field, refused — from a body that
// simply omits it. A plain []string flattens those two into the same nil, which
// would let a participant send an owner-only key and receive a 200 for it.
type updateTripBody struct {
	Name                 *string   `json:"name"`
	EndsAt               *string   `json:"endsAt"`
	AddParticipantIDs    *[]string `json:"addParticipantIds"`
	RemoveParticipantIDs *[]string `json:"removeParticipantIds"`
}

// ownerOnlyFieldPresent reports whether the body carries anything a live
// participant may not change (MYR-618).
//
// PRESENCE, NOT VALUE: `{"removeParticipantIds": []}` counts. It is the client
// asking to exercise an owner's verb, and the answer to that must not depend on
// whether the request happened to be a no-op — a rule that refused only the
// requests that would have DONE something would be a rule nobody could state.
//
// An explicit JSON `null` is the one spelling that does NOT count, and that is
// the contract's own doing: §7.30.4 defines an absent key as UNCHANGED, the
// owner's own path treats `{"name": null}` identically to omitting it, and the
// schema permits null on none of these fields. Refusing it here would make the
// participant branch stricter than the owner branch about a value that means
// nothing on either.
func (b updateTripBody) ownerOnlyFieldPresent() bool {
	return b.Name != nil || b.EndsAt != nil || b.RemoveParticipantIDs != nil
}

// addedShareIDs is the add list, flattened. Nil for an absent key and for an
// explicit empty list alike — by then the presence question has already been
// asked and answered.
func (b updateTripBody) addedShareIDs() []string {
	if b.AddParticipantIDs == nil {
		return nil
	}
	return *b.AddParticipantIDs
}

func (h *TripHandler) parseUpdate(w http.ResponseWriter, body updateTripBody) (TripUpdateInput, bool) {
	in := TripUpdateInput{
		Name:              body.Name,
		AddParticipantIDs: body.addedShareIDs(),
	}
	if body.RemoveParticipantIDs != nil {
		in.RemoveParticipantIDs = *body.RemoveParticipantIDs
	}
	if body.EndsAt != nil {
		endsAt, ok := h.parseInstant(w, "endsAt", *body.EndsAt)
		if !ok {
			return TripUpdateInput{}, false
		}
		in.EndsAt = &endsAt
	}
	return in, true
}

// parseInstant reads one RFC 3339 field, naming it in the refusal.
func (h *TripHandler) parseInstant(w http.ResponseWriter, field, raw string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			field+" must be an RFC 3339 instant")
		return time.Time{}, false
	}
	return t, true
}
