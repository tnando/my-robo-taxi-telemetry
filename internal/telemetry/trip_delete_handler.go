package telemetry

import (
	"net/http"
	"time"
)

// DELETE /api/trips/{tripId} — the owner deletes a trip (MYR-607, §7.30.10).
//
// THE TENTH ROUTE, and the only one that removes a trip rather than changing
// it. Its three rules are the surface's own, applied to the most destructive
// operation on it:
//
//   - OWNER ONLY, 404 TO EVERYBODY ELSE — a participant, a stranger and an
//     unknown id receive the identical answer, so the endpoint tells nobody
//     which trip ids are real. Never 403.
//   - ANY STATUS. Scheduled, active and ended are all deletable, because the
//     product question the owner is answering is "I do not want this trip on
//     my list", and a trip that ended yesterday is exactly the one they most
//     often mean. §7.30.4's `trip_ended` refusal is about MUTATING a finished
//     window — extending it would resurrect live access — and a deletion
//     grants nothing to anybody.
//   - THE DRIVES SURVIVE. A trip never owned a drive; the window merely
//     selected one, by time, from the car's own history. That is the sentence
//     the client's confirm dialog puts in front of the owner, and it is true
//     because this handler deletes four tables and none of them is "Drive".

// ServeDelete handles DELETE /api/trips/{tripId} — OWNER ONLY, 204, any status.
//
// ⚠ SETTLE FIRST, DELETE SECOND, AND THE ORDER IS NORMATIVE. The settlement
// reads the trip — its roster, its open leg, each party's Live Activity
// registration — and every one of those rows is about to stop existing. Run the
// other way round, a participant's lock screen would keep a live card for a
// journey that no longer exists, addressed by a token nothing can look up any
// more, until ActivityKit's own staleness ceiling retired it hours later.
//
// The cost of that order is the failure mode it creates, and it is worth
// stating rather than discovering: if the DELETE then fails, the trip has been
// SETTLED but not removed — ended, announced, cards down — and the client's
// retry deletes it. That is the conservative direction. Access is already
// revoked, nobody is told anything false, and the only visible artefact is a
// trip that briefly reads as `ended` on the owner's own list. The reverse
// ordering's failure mode is a stranded card on somebody else's phone, which
// nothing can clear.
func (h *TripHandler) ServeDelete(w http.ResponseWriter, r *http.Request) {
	tripID, ctx, userID, ok := h.beginTrip(w, r)
	if !ok {
		return
	}

	// READ FIRST, for the same two reasons ServeEnd reads first: the read is
	// the 404 (for an unknown trip, and for one the caller is not on), and it
	// is the only chance to learn who to notify. The store's own owner-scoped
	// delete is what actually DECIDES — this read cannot be trusted as a gate
	// on its own, because a trip could change hands between the two — but a
	// read that refuses here produces the right answer without a write.
	before, err := h.trips.GetTrip(ctx, tripID, userID)
	if err != nil {
		h.failTrip(w, "delete", tripID, err)
		return
	}
	if before.Role != tripRoleOwner {
		// A PARTICIPANT GETS 404, not 403, and they can SEE this trip through
		// GET — the same deliberate oddity §7.30.4 accepts for PATCH. A rule
		// with one exception is a rule somebody applies inconsistently next
		// time, and the exception here would be the delete route.
		h.writeTripError(w, ErrTripNotFound)
		return
	}

	// SETTLEMENT IS SKIPPED FOR A TRIP THAT IS ALREADY OVER, so a tidy-up of
	// last month's trips does not put a banner on anybody's phone. The `trips`
	// notifier's own end claim would refuse the second announcement anyway —
	// every path that ends a trip stamps `ended_notified_at` — but a handler
	// that relied on that would be relying on a stamp it does not own, and the
	// day a trip ends without one is the day this route wakes six people up to
	// tell them about a road trip they finished in August. The two guards are
	// belt and braces, exactly as they are on §7.30.5: the status gate answers
	// for a trip that was already over when the request arrived, and the claim
	// answers for one that ended WHILE it was in flight — an owner tapping End
	// and Delete in the same second, which settles once and announces once.
	if tripStatusOf(before, time.Now()) != tripStatusEnded {
		h.notifier.TripDeleted(ctx, before, participantUserIDs(before))
	}

	if err := h.trips.DeleteTrip(ctx, tripID, userID); err != nil {
		h.failTrip(w, "delete", tripID, err)
		return
	}

	// 204 AND NO BODY. There is nothing left to describe: the trip the client
	// would have decoded is gone, and returning its last state would invite a
	// cache to keep it. A SECOND CALL IS A 404 — the store finds no row — which
	// from the client's side is indistinguishable from success and is meant to
	// be: a delete that answered 404 on the retry of its own timed-out request
	// would be a bug the app could not tell from a bug in the server, so the
	// contract says to treat 404 here as done.
	w.WriteHeader(http.StatusNoContent)
}
