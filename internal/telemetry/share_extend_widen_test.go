package telemetry

import (
	"errors"
	"net/http"
	"testing"
)

// MYR-609 handler side of the WIDENING signal: which owner action announces
// that a grantee GAINED a car, and in what order relative to the cache bust.
// The hub-side behavior is covered in internal/ws; the mirror of
// share_invite_teardown_test.go, for the mirror-image direction.

// recordingWidener captures ShareAccessWidener calls and — because the ordering
// against the cache bust is a correctness property, not a stylistic one —
// samples the invalidator's state at the moment each widening arrives.
type recordingWidener struct {
	calls []widenerCall
	inv   *fakeAccessInvalidator
}

type widenerCall struct {
	granteeUserID  string
	vehicleID      string
	reason         string
	bustedThisUser bool
}

func (wd *recordingWidener) ShareAccessWidened(granteeUserID, vehicleID, reason string) {
	call := widenerCall{granteeUserID: granteeUserID, vehicleID: vehicleID, reason: reason}
	if wd.inv != nil {
		for _, u := range wd.inv.busted {
			if u == granteeUserID {
				call.bustedThisUser = true
			}
		}
	}
	wd.calls = append(wd.calls, call)
}

// newWidenMux mounts the extend route with both an invalidator and a widener.
func newWidenMux(t *testing.T, store ShareInviteStore, inv *fakeAccessInvalidator, wd ShareAccessWidener) *http.ServeMux {
	t.Helper()
	h := NewShareInviteHandler(
		&stubTokenValidator{userID: shareOwnerUser},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
		store,
		inv,
		testShareLinkSigner(t),
		discardLogger(),
		WithShareAccessWidener(wd),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/vehicles/{vehicleId}/share/extend", h.ServeExtend)
	return mux
}

const widenBody = `{"shareId":"csh0123456789abcdef0123456789abcd"}`

// A successful extend must re-handshake the GRANTEE's live sessions. Without
// it, `Client.vehicleIDs` stays frozen at whatever the handshake read, so the
// person the car was just shared with does not get it until they happen to
// reconnect — while the owner is told the share worked and the grantee's own
// REST surface already lists it.
func TestExtendWidensTheGranteesLiveSocket(t *testing.T) {
	store := &fakeShareInviteStore{extended: extendedGrantRow(), extendee: shareViewerUser}
	inv := &fakeAccessInvalidator{}
	widener := &recordingWidener{inv: inv}

	rec := doShareRequest(t, newWidenMux(t, store, inv, widener),
		http.MethodPost, shareExtendPath, widenBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	if len(widener.calls) != 1 {
		t.Fatalf("widener called %d times, want 1", len(widener.calls))
	}
	got := widener.calls[0]
	if got.granteeUserID != shareViewerUser {
		t.Errorf("widened user = %q, want the GRANTEE %q — the owner's own sessions never "+
			"needed re-handshaking, they did not gain anything", got.granteeUserID, shareViewerUser)
	}
	if got.vehicleID != shareFixtureVeh {
		t.Errorf("vehicle = %q, want the PATH vehicle %q", got.vehicleID, shareFixtureVeh)
	}
	if got.reason != "extended" {
		t.Errorf("reason = %q, want %q", got.reason, "extended")
	}

	// THE BUST MUST ALREADY HAVE HAPPENED. The widening provokes a reconnect,
	// and a handshake served from a stale access set comes back WITHOUT the
	// car — a no-op that looks like a fix.
	if !got.bustedThisUser {
		t.Error("the grantee's cached access set had not been busted when the widening was " +
			"announced; their reconnect would be served the PRE-extend set and come back " +
			"without the car it was sent to collect")
	}
}

// Nothing was granted, so nothing is announced. A widening on a refused extend
// would disconnect every session a grantee holds to tell them about access they
// did not get.
func TestExtendDoesNotWidenOnRefusal(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"already shared", ErrShareAlreadyGranted},
		{"the source is paused", ErrShareSourceSuspended},
		{"the grantee is paused on the target", ErrShareTargetSuspended},
		{"the grantee left the car", ErrShareGranteeLeft},
		{"an unclassified failure", errors.New("boom")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeShareInviteStore{extendErr: tt.err}
			inv := &fakeAccessInvalidator{}
			widener := &recordingWidener{inv: inv}

			doShareRequest(t, newWidenMux(t, store, inv, widener),
				http.MethodPost, shareExtendPath, widenBody)

			if len(widener.calls) != 0 {
				t.Errorf("widener called %+v on a refused extend, want not at all", widener.calls)
			}
		})
	}
}

// The widener is OPTIONAL, like the invalidator and the notifier beside it: a
// deployment without one still grants the car, and the grantee's open socket
// picks it up at its next reconnect. The 201 must not depend on it.
func TestExtendSucceedsWithNoWidenerConfigured(t *testing.T) {
	store := &fakeShareInviteStore{extended: extendedGrantRow(), extendee: shareViewerUser}
	inv := &fakeAccessInvalidator{}
	mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, inv)

	rec := doShareRequest(t, mux, http.MethodPost, shareExtendPath, widenBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if len(inv.busted) != 1 {
		t.Errorf("busted = %v, want the grantee — the cache bust is the half that does not "+
			"depend on the optional widener", inv.busted)
	}
}

// An extend whose store could not name the grantee widens nobody. An empty user
// id is not a wildcard, and the guard is in the handler rather than left to the
// hub so a mistake here cannot become "re-handshake everybody".
func TestExtendWithNoGranteeWidensNobody(t *testing.T) {
	store := &fakeShareInviteStore{extended: extendedGrantRow()} // no extendee
	inv := &fakeAccessInvalidator{}
	widener := &recordingWidener{inv: inv}

	rec := doShareRequest(t, newWidenMux(t, store, inv, widener),
		http.MethodPost, shareExtendPath, widenBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if len(widener.calls) != 0 {
		t.Errorf("widener called %+v for an unnamed grantee, want not at all", widener.calls)
	}
	if len(inv.busted) != 0 {
		t.Errorf("busted = %v, want none for an unnamed grantee", inv.busted)
	}
}
