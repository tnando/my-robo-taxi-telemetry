package telemetry

import (
	"net/http"
	"testing"
)

// MYR-601 — the §7.24 group-ride join is the third path that busted the cache
// and stopped there. A joiner who tapped the link inside the app is CONNECTED,
// and the 200 sends them straight to the rider tracking surface — which cannot
// subscribe to the car until their frozen session is replaced.

// newJoinWidenHandler wires both halves of the join's access change.
func newJoinWidenHandler(store RideRequestStore, inv AccessCacheInvalidator, wd ShareAccessWidener) *RideRequestHandler {
	return NewRideRequestHandler(
		&stubTokenValidator{userID: groupJoinerID},
		&stubVehicleSnapshotReader{},
		store,
		nil,
		discardLogger(),
		WithRideAccessInvalidator(inv),
		WithRideAccessWidener(wd),
	)
}

func TestRideJoinWidensTheJoinersLiveSocket(t *testing.T) {
	joined := groupRideData(rideStatusAccepted)
	joined.Members = []RideMemberData{{UserID: groupJoinerID, FirstName: "Sam"}}
	inv := &fakeAccessInvalidator{}
	widener := &recordingWidener{inv: inv}

	res := postJoin(newJoinWidenHandler(
		&fakeRideStore{joinRec: joined, joinCreated: true}, inv, widener), `{"code":"RBO246"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", res.Code, res.Body.String())
	}

	if len(widener.calls) != 1 {
		t.Fatalf("widener called %d times, want 1", len(widener.calls))
	}
	got := widener.calls[0]
	if got.granteeUserID != groupJoinerID {
		t.Errorf("widened user = %q, want the JOINER %q", got.granteeUserID, groupJoinerID)
	}
	if got.vehicleID != joined.VehicleID {
		t.Errorf("vehicle = %q, want the ride's car %q", got.vehicleID, joined.VehicleID)
	}
	if got.reason != "ride_joined" {
		t.Errorf("reason = %q, want %q", got.reason, "ride_joined")
	}
	if !got.bustedThisUser {
		t.Error("the joiner's cached access set had not been busted when the widening was " +
			"announced; their reconnect would be served the PRE-join set")
	}
}

// AN IDEMPOTENT RE-JOIN ANNOUNCES NOTHING. The membership row already existed,
// so nothing widened — and re-handshaking every session the person holds each
// time they re-open a link they already redeemed would be a disconnect for no
// change at all. It is the same `created` gate the cache bust and the
// `ride_member_joined` publish already sit behind.
func TestRideRejoinAnnouncesNothing(t *testing.T) {
	joined := groupRideData(rideStatusAccepted)
	joined.Members = []RideMemberData{{UserID: groupJoinerID, FirstName: "Sam"}}
	inv := &fakeAccessInvalidator{}
	widener := &recordingWidener{inv: inv}

	res := postJoin(newJoinWidenHandler(
		&fakeRideStore{joinRec: joined, joinCreated: false}, inv, widener), `{"code":"RBO246"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", res.Code, res.Body.String())
	}
	if len(widener.calls) != 0 || len(inv.busted) != 0 {
		t.Errorf("a re-join announced %+v / %v, want nothing", widener.calls, inv.busted)
	}
}

// A REFUSED JOIN ANNOUNCES NOTHING — nobody gained a car, and the widening
// would disconnect the caller to tell them about access they did not get.
func TestRideJoinRefusalNeverWidens(t *testing.T) {
	inv := &fakeAccessInvalidator{}
	widener := &recordingWidener{inv: inv}

	res := postJoin(newJoinWidenHandler(
		&fakeRideStore{joinErr: ErrRideJoinSelfParty}, inv, widener), `{"code":"RBO246"}`)
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", res.Code, res.Body.String())
	}
	if len(widener.calls) != 0 {
		t.Errorf("a refused join announced %+v, want nothing", widener.calls)
	}
}
