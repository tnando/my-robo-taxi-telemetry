package telemetry

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Handler tests for DELETE /api/trips/{tripId} (MYR-607, §7.30.10).
//
// The store is the same hand-written fake the rest of the surface uses. What is
// asserted here is the HANDLER's contract — which caller gets which status, what
// the notifier is told, and in what ORDER the two calls happen. The four tables
// actually emptying is asserted against a real Postgres in internal/store.

// endedFixtureTrip is fixtureTrip with its window closed an hour ago.
func endedFixtureTrip() TripData {
	trip := fixtureTrip()
	ended := time.Now().Add(-time.Hour)
	trip.StartsAt = time.Now().Add(-6 * time.Hour)
	trip.EndsAt = time.Now().Add(-2 * time.Hour)
	trip.EndedAt = &ended
	return trip
}

// TestTripDeleteOwnerGets204 is the happy path: the owner deletes an active
// trip, the store is asked once, and nothing is returned.
func TestTripDeleteOwnerGets204(t *testing.T) {
	store := &fakeTripStore{trip: fixtureTrip()}
	notifier := &recordingTripNotifier{}
	handler := newTripTestHandler(t, store, true, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204. Body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty — the trip the client would decode is gone", rec.Body.String())
	}
	if store.tripDeleteCalls != 1 {
		t.Fatalf("DeleteTrip calls = %d, want 1", store.tripDeleteCalls)
	}
}

// TestTripDeleteSettlesBeforeDeleting pins the ORDERING that the whole route
// hangs on.
//
// The settlement reads the roster, the open leg and every party's Live Activity
// registration, and all of them are about to stop existing. Run the other way
// round, a participant's lock screen keeps a card for a journey that no longer
// exists, addressed by a token nothing can look up any more.
func TestTripDeleteSettlesBeforeDeleting(t *testing.T) {
	store := &fakeTripStore{trip: fixtureTrip()}
	var order []string
	notifier := &recordingTripNotifier{onDeleted: func() {
		order = append(order, "settle")
		if store.tripDeleteCalls != 0 {
			t.Errorf("the trip was already deleted when the settlement ran")
		}
	}}
	handler := newTripTestHandler(t, store, true, WithTripNotifier(notifier))

	if rec := tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	order = append(order, "delete")

	if len(order) != 2 || order[0] != "settle" || order[1] != "delete" {
		t.Fatalf("order = %v, want [settle delete]", order)
	}
	if len(notifier.deleted) != 1 {
		t.Fatalf("TripDeleted calls = %d, want 1", len(notifier.deleted))
	}
	if got := notifier.deleted[0]; len(got) != 1 || got[0] != tripTestParticipant {
		t.Errorf("audience = %v, want the one participant", got)
	}
	// THE OWNER IS NOT IN THE AUDIENCE. They pressed the button; a phone that
	// buzzes to tell its owner about their own tap is how a category gets
	// switched off.
	for _, id := range notifier.deleted[0] {
		if id == tripTestOwner {
			t.Errorf("the owner was notified about their own deletion")
		}
	}
}

// TestTripDeleteOfAnEndedTripNotifiesNobody covers the tidy-up case: deleting
// last month's trips must not put a banner on anybody's phone.
//
// ANY STATUS IS DELETABLE — that is the route's whole point — but a trip whose
// window is already closed has already told its participants it ended, and a
// second announcement about the same fact is noise.
func TestTripDeleteOfAnEndedTripNotifiesNobody(t *testing.T) {
	store := &fakeTripStore{trip: endedFixtureTrip()}
	notifier := &recordingTripNotifier{}
	handler := newTripTestHandler(t, store, true, WithTripNotifier(notifier))

	if rec := tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 — an ended trip is deletable", rec.Code)
	}
	if len(notifier.deleted) != 0 {
		t.Errorf("TripDeleted calls = %d, want 0 for a trip that had already ended", len(notifier.deleted))
	}
	if store.tripDeleteCalls != 1 {
		t.Errorf("DeleteTrip calls = %d, want 1 — the row still goes", store.tripDeleteCalls)
	}
}

// TestTripDeleteRefusesEveryoneButTheOwner is the 404-not-403 rule on the most
// destructive route of the surface.
//
// A PARTICIPANT AND A STRANGER GET THE SAME ANSWER, and it is the same one an
// unknown id gets. The participant case is the deliberately odd one: they can
// SEE this trip through GET, so a 404 from DELETE reads strangely — and it is
// still right, because a rule with one exception is a rule somebody applies
// inconsistently next time.
func TestTripDeleteRefusesEveryoneButTheOwner(t *testing.T) {
	participantView := fixtureTrip()
	participantView.Role = "participant"

	cases := []struct {
		name  string
		store *fakeTripStore
	}{
		{
			// The store answers the read (they are on the trip) with the
			// participant role; the handler refuses on the role.
			name:  "a participant",
			store: &fakeTripStore{trip: participantView},
		},
		{
			// The store's own read refuses: a stranger, and an unknown id, are
			// one sentinel by design.
			name:  "a stranger, and an unknown trip",
			store: &fakeTripStore{err: ErrTripNotFound},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notifier := &recordingTripNotifier{}
			handler := newTripTestHandler(t, tc.store, true, WithTripNotifier(notifier))

			rec := tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (never 403). Body: %s", rec.Code, rec.Body.String())
			}
			if got := decodeTripError(t, rec).Code; got != wserrors.ErrCodeNotFound {
				t.Errorf("code = %q, want not_found", got)
			}
			if tc.store.tripDeleteCalls != 0 {
				t.Errorf("DeleteTrip calls = %d, want 0 — a refusal must not write", tc.store.tripDeleteCalls)
			}
			if len(notifier.deleted) != 0 {
				t.Errorf("a refused delete settled the trip")
			}
		})
	}
}

// TestTripDeleteSecondCallIs404 is the idempotency the client is told to read
// as success.
//
// The store finds no row and returns the same sentinel a stranger gets. A
// delete that answered 404 on the retry of its own timed-out request would be a
// bug the app could not tell from a bug in the server, so §7.30.10 says to
// treat it as done.
func TestTripDeleteSecondCallIs404(t *testing.T) {
	store := &fakeTripStore{trip: fixtureTrip()}
	handler := newTripTestHandler(t, store, true)

	if rec := tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("first call status = %d, want 204", rec.Code)
	}

	// The row is gone, so every read of it now refuses — which is what the real
	// store does and what this fake is switched to.
	store.err = ErrTripNotFound
	rec := tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second call status = %d, want 404", rec.Code)
	}
	if store.tripDeleteCalls != 1 {
		t.Errorf("DeleteTrip calls = %d, want 1 — the second call never reached the write", store.tripDeleteCalls)
	}
}

// TestTripDeleteReportsAStoreFailure covers the failure mode the settle-first
// ordering creates: the trip has been settled and the delete then fails.
//
// It is a 500, the client retries, and the retry deletes it. That is the
// conservative direction — access is already revoked and nothing false has been
// said — where the reverse ordering's failure is a stranded card on somebody
// else's phone that nothing can clear.
func TestTripDeleteReportsAStoreFailure(t *testing.T) {
	store := &fakeTripStore{trip: fixtureTrip(), deleteErr: errors.New("connection refused")}
	notifier := &recordingTripNotifier{}
	handler := newTripTestHandler(t, store, true, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := decodeTripError(t, rec).Code; got != wserrors.ErrCodeInternalError {
		t.Errorf("code = %q, want internal_error", got)
	}
	if len(notifier.deleted) != 1 {
		t.Errorf("the settlement did not run before the failed delete")
	}
}

// TestTripDeleteWithNoNotifierStillDeletes is the seam's load-bearing property
// applied to this route: nil is a no-op, not a failure.
func TestTripDeleteWithNoNotifierStillDeletes(t *testing.T) {
	store := &fakeTripStore{trip: fixtureTrip()}
	handler := newTripTestHandler(t, store, true)

	if rec := tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 with no notifier wired", rec.Code)
	}
	if store.tripDeleteCalls != 1 {
		t.Errorf("DeleteTrip calls = %d, want 1", store.tripDeleteCalls)
	}
}
