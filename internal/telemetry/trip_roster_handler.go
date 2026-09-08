package telemetry

import (
	"context"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// MYR-618: the two halves of "anybody on the trip can bring somebody along".
//
//	PATCH /api/trips/{tripId}          the participant branch (§7.30.4)
//	GET   /api/trips/{tripId}/addable-people   (§7.30.11)
//
// THE PRODUCT RULE IN ONE SENTENCE: a live participant may add any person who
// ALREADY holds an accepted, unsuspended grant on the trip's vehicle, and may
// do nothing else. Removing, renaming, moving the window, ending and deleting
// all stay with the owner.
//
// WHY THAT IS SAFE, stated once here because it is the whole argument: a trip
// mints no vehicle relationship. Its participants are chosen from the car's
// already-accepted grants, so the widest thing a participant can do is move
// somebody the OWNER already trusted with the car from "sees it when the owner
// shares it" to "sees it during this window". They cannot invite a stranger,
// and the window they are widening is one the owner opened.

// patchAsParticipant is the participant branch of §7.30.4.
//
// ⚠ THE WHOLE REQUEST IS REFUSED, NOTHING IS APPLIED, when the body carries any
// owner-only field — even alongside a perfectly legal `addParticipantIds`. A
// partial apply would be the worst of the three available answers: the client
// asked for two things, got a 200, and one of them silently did not happen.
func (h *TripHandler) patchAsParticipant(
	ctx context.Context, w http.ResponseWriter, tripID, userID string, before TripData, body updateTripBody,
) {
	if body.ownerOnlyFieldPresent() {
		// 403, NOT 404, and this is the one place on §7.30 where that is right.
		// The 404-not-403 rule exists so a trip id cannot be probed; this
		// caller has already read the trip through the same handler, so there
		// is nothing left to conceal and a 404 would only be a lie about a
		// resource they are demonstrably on. The message names the verbs
		// rather than the field, because the client's fix is to stop offering
		// the button, not to rename a key.
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied,
			"only the trip's owner can rename it, change its window, or remove people")
		return
	}

	trip, err := h.trips.AddTripParticipants(ctx, tripID, userID, body.addedShareIDs())
	if err != nil {
		h.failTrip(w, "patch", tripID, err)
		return
	}

	added := newParticipantUserIDs(before, trip)
	if len(added) > 0 {
		// The people who were just added hear the SAME `trip_added` the owner's
		// add sends. Nothing about the banner changes with who did the adding —
		// what changed for them is identical either way.
		h.notifier.TripAdded(ctx, trip, added)

		// And the OWNER hears that somebody else widened their roster. It is
		// the only push on this surface whose recipient is the owner alone, and
		// it exists because this is the only thing on the platform that changes
		// who can watch an owner's car without the owner doing it.
		//
		// THE NAMES COME FROM THE ROSTER WE JUST READ BACK, not from a second
		// lookup: they are already resolved by the roster's own rule (confirmed
		// first name, else the owner's label for the grant), so the banner
		// cannot call somebody one thing while the trip sheet calls them
		// another.
		h.notifier.TripParticipantAdded(ctx, trip, participantNameFor(trip, userID), participantNamesFor(trip, added))
	}
	// An add of somebody already on the trip lands here with `added` empty: no
	// push, no audit row, and a 200 carrying the roster the caller asked for.
	// It is a no-op that succeeded, which is what the client meant.

	h.writeJSON(w, http.StatusOK, tripWire(trip, userID))
}

// ServeAddablePeople handles GET /api/trips/{tripId}/addable-people — §7.30.11.
//
// IT EXISTS BECAUSE §7.5's GRANT LISTING IS OWNER-ONLY and must stay that way.
// A participant who may add people needs to know who is addable, and the
// owner's own share list is the wrong place to get it. What §7.5 carries and
// this route WITHHOLDS, stated exactly (review finding 6):
//
//	invite CODES        a credential — anybody holding one can redeem the grant
//	email addresses     the invitee's, a P1 identifier this platform never
//	                    publishes to a third party
//	PENDING invitations rows for people who have not accepted and may never,
//	                    which are the owner's business alone
//	per-grant permissions and statuses
//	                    what each person may do with the car, and whether the
//	                    grant is suspended — the owner's own administration
//	user ids            durable cross-surface identifiers, absent from every
//	                    trip surface by the same rule that keeps them off the
//	                    roster
//
// ⚠ WHAT IT DOES NOT WITHHOLD IS THE OWNER'S LABEL, and that is deliberate
// rather than an oversight. `displayName` is the roster's own ladder — the
// accepting account's CONFIRMED first name, falling back to the label the owner
// typed when they issued the grant — so an owner who invited somebody as "Mom"
// before she was through the naming prompt shows "Mom" here, to a participant.
// The label is therefore the documented fallback on this surface, exactly as it
// is on `participants[].name` (§7.30.3), and for the same reason: a person must
// not be called one thing in the picker and another on the trip they were added
// to one tap later, and a blank row is worse for everybody than a nickname.
// **The confirmed name wins whenever there is one, and the naming prompt makes
// that the common case** — this is a fallback, not the ordinary answer. An
// owner who wants a label kept private should not use it to name a person.
//
// The OWNER reads it too, through the same route. One list, built by one
// statement, so the owner's picker and a participant's picker cannot come to
// offer different people — WITH ONE EXCEPTION the store owns: somebody the
// owner REMOVED from this trip is withheld from a participant's picker and
// still offered to the owner's, because the owner is the only person who may
// undo their own removal (migration 0061, §7.30.4).
func (h *TripHandler) ServeAddablePeople(w http.ResponseWriter, r *http.Request) {
	tripID, ctx, userID, ok := h.beginTrip(w, r)
	if !ok {
		return
	}

	people, err := h.trips.TripAddablePeople(ctx, tripID, userID)
	if err != nil {
		h.failTrip(w, "addable-people", tripID, err)
		return
	}

	// `{people: [...]}` with NO cursor, the same envelope decision §7.30.2
	// makes: a car has a handful of share-holders, not a feed, and an SDK
	// pagination helper must not mistake this for a page and go looking for a
	// cursor that will never be there.
	items := make([]map[string]any, 0, len(people))
	for _, p := range people {
		items = append(items, map[string]any{
			"shareId":     p.ShareID,
			"displayName": p.Name,
		})
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"people": items})
}

// participantNameFor returns the roster name of one user, or "" when they are
// not on the roster.
//
// The empty string is a real case rather than a defensive one: the OWNER holds
// no participant row, so an owner id looked up here finds nothing. Every caller
// of this function has already established that its subject is a participant,
// and the copy layer falls back rather than rendering an empty sentence.
func participantNameFor(trip TripData, userID string) string {
	for _, p := range trip.Participants {
		if p.UserID == userID {
			return p.Name
		}
	}
	return ""
}

// participantNamesFor returns the roster names of the given users, in the
// roster's own order rather than the caller's.
//
// THE ROSTER'S ORDER, deliberately: it is `added_at, user_id`, which is stable
// across reads, so a banner naming two people names them the same way twice if
// the same push is ever re-rendered. The caller's order is whatever the diff
// happened to produce.
func participantNamesFor(trip TripData, userIDs []string) []string {
	want := make(map[string]bool, len(userIDs))
	for _, id := range userIDs {
		want[id] = true
	}
	out := make([]string, 0, len(userIDs))
	for _, p := range trip.Participants {
		if want[p.UserID] {
			out = append(out, p.Name)
		}
	}
	return out
}
