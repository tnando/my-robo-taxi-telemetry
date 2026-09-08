package telemetry

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// MYR-601 handler side of the WIDENING signal for the two sharing paths that
// had a cache bust and nothing else: §7.5.5 redeem, and the un-suspend half of
// §7.5.4 PATCH. Both are the same finding as MYR-609's extend — the bust fixes
// the NEXT handshake, and a person who is already connected does not make one.

// newRedeemWidenMux mounts the redeem route with both halves wired.
func newRedeemWidenMux(t *testing.T, redeem ShareRedeemStore, lister SharedVehicleLister,
	inv AccessCacheInvalidator, wd ShareAccessWidener) *http.ServeMux {
	t.Helper()
	h := NewShareRedeemHandler(&stubTokenValidator{userID: shareViewerUser}, redeem, lister, inv,
		discardLogger(), WithShareRedeemWidener(wd))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/invites/redeem", h.ServeHTTP)
	return mux
}

// THE "YOU'RE IN!" SCREEN FOLLOWED BY A DEAD MAP. A redeemer who tapped the
// invite link inside the app is connected, so their session's access set was
// frozen before the grant existed and the cache bust fixes a handshake they are
// not about to make.
func TestRedeemWidensTheRedeemersLiveSocket(t *testing.T) {
	redeem := &fakeShareRedeemStore{
		grants:    []ShareGrantRow{{VehicleID: fixtureSnapshotRowID, OwnerUserID: shareOwnerUser}},
		ownerName: "Alex",
	}
	lister := &fakeSharedLister{rows: []SharedVehicleRow{sharedCatalogRow(false)}}
	inv := &fakeAccessInvalidator{}
	widener := &recordingWidener{inv: inv}

	rec := doShareRequest(t, newRedeemWidenMux(t, redeem, lister, inv, widener),
		http.MethodPost, redeemPath, `{"code":"RBO246"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	if len(widener.calls) != 1 {
		t.Fatalf("widener called %d times, want 1", len(widener.calls))
	}
	got := widener.calls[0]
	if got.granteeUserID != shareViewerUser {
		t.Errorf("widened user = %q, want the REDEEMER %q", got.granteeUserID, shareViewerUser)
	}
	if got.vehicleID != fixtureSnapshotRowID {
		t.Errorf("vehicle = %q, want %q", got.vehicleID, fixtureSnapshotRowID)
	}
	if got.reason != "redeemed" {
		t.Errorf("reason = %q, want %q", got.reason, "redeemed")
	}
	// Same order rule as every other widening: a re-handshake served from the
	// pre-redemption set comes back without the car.
	if !got.bustedThisUser {
		t.Error("the redeemer's cached access set had not been busted when the widening was " +
			"announced; their reconnect would be served the PRE-redemption set")
	}
}

// A MULTI-CAR INVITE PUBLISHES ONE SIGNAL, NOT ONE PER CAR. WidenUserAccess
// re-handshakes every session the user holds — it cannot find them by a vehicle
// they are not yet authorized for, which is the whole reason it exists — so one
// publish already covers every car the redemption granted. Publishing per car
// would close the same sessions again for each one.
func TestRedeemWidensOncePerRedemption(t *testing.T) {
	redeem := &fakeShareRedeemStore{
		grants: []ShareGrantRow{
			{VehicleID: fixtureSnapshotRowID, OwnerUserID: shareOwnerUser},
			{VehicleID: "cveh_second_car_0000000000", OwnerUserID: shareOwnerUser},
		},
		ownerName: "Alex",
	}
	lister := &fakeSharedLister{rows: []SharedVehicleRow{sharedCatalogRow(false)}}
	inv := &fakeAccessInvalidator{}
	widener := &recordingWidener{inv: inv}

	rec := doShareRequest(t, newRedeemWidenMux(t, redeem, lister, inv, widener),
		http.MethodPost, redeemPath, `{"code":"RBO246"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(widener.calls) != 1 {
		t.Fatalf("widener called %d times for a two-car redemption, want exactly 1", len(widener.calls))
	}
}

// A REFUSED REDEMPTION ANNOUNCES NOTHING. Nobody gained anything, and a
// widening would disconnect every session the caller holds to tell them about
// access they did not get.
func TestRedeemDoesNotWidenOnRefusal(t *testing.T) {
	redeem := &fakeShareRedeemStore{redeemErr: fmt.Errorf("stub: %w", sdk.ErrNotFound)}
	lister := &fakeSharedLister{}
	inv := &fakeAccessInvalidator{}
	widener := &recordingWidener{inv: inv}

	rec := doShareRequest(t, newRedeemWidenMux(t, redeem, lister, inv, widener),
		http.MethodPost, redeemPath, `{"code":"RBO246"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a refusal", rec.Code)
	}
	if len(widener.calls) != 0 {
		t.Errorf("a refused redemption announced %+v, want nothing", widener.calls)
	}
}

// A REDEEM HANDLER WITH NO WIDENER STILL REDEEMS. The seam is optional
// everywhere — dev mode and every test that wires no bus get nil — and the
// degraded behavior is the pre-MYR-601 one, not a failure.
func TestRedeemWithoutAWidenerStillSucceeds(t *testing.T) {
	redeem := &fakeShareRedeemStore{
		grants:    []ShareGrantRow{{VehicleID: fixtureSnapshotRowID, OwnerUserID: shareOwnerUser}},
		ownerName: "Alex",
	}
	lister := &fakeSharedLister{rows: []SharedVehicleRow{sharedCatalogRow(false)}}
	inv := &fakeAccessInvalidator{}

	rec := doShareRequest(t, newRedeemWidenMux(t, redeem, lister, inv, nil),
		http.MethodPost, redeemPath, `{"code":"RBO246"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(inv.busted) != 1 {
		t.Errorf("cache busts = %v, want the redeemer's — the pre-MYR-601 half must survive", inv.busted)
	}
}

// --- MYR-601: the un-suspend half of §7.5.4 PATCH ---------------------------

// newPatchWidenMux mounts PATCH with BOTH socket seams, which is the only way
// the asymmetry between them can be asserted: suspend must close, un-suspend
// must widen, and a patch that touched neither must do nothing at all.
func newPatchWidenMux(t *testing.T, store ShareInviteStore, inv *fakeAccessInvalidator,
	notifier ShareAccessNotifier, wd ShareAccessWidener) *http.ServeMux {
	t.Helper()
	h := NewShareInviteHandler(
		&stubTokenValidator{userID: shareOwnerUser},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
		store,
		inv,
		testShareLinkSigner(t),
		discardLogger(),
		WithShareAccessNotifier(notifier),
		WithShareAccessWidener(wd),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/invites/{inviteId}", h.ServePatch)
	return mux
}

// SUSPENSION IS THE ONE FLAG ON THIS ENDPOINT THAT MOVES THE CAR IN AND OUT OF
// THE ACCESS SET, so lifting one is a widening — and the viewer whose socket
// was frozen while they were suspended would otherwise stay dark until they
// reconnected, on a grant the owner has already restored.
func TestUnsuspendWidensTheGranteesLiveSocket(t *testing.T) {
	store := &fakeShareInviteStore{patched: patchedGrantRow(true, false), patchee: shareViewerUser}
	inv := &fakeAccessInvalidator{}
	notifier := &recordingNotifier{inv: inv}
	widener := &recordingWidener{inv: inv}

	rec := doShareRequest(t, newPatchWidenMux(t, store, inv, notifier, widener),
		http.MethodPatch, sharePatchPath, `{"suspended":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	if len(widener.calls) != 1 {
		t.Fatalf("widener called %d times, want 1", len(widener.calls))
	}
	got := widener.calls[0]
	if got.granteeUserID != shareViewerUser || got.reason != "unsuspended" {
		t.Errorf("widening = %+v, want the grantee with reason unsuspended", got)
	}
	if !got.bustedThisUser {
		t.Error("the grantee's cached access set had not been busted when the widening was announced")
	}
	// AND NOTHING WAS TORN DOWN. The two directions must never both fire.
	if len(notifier.calls) != 0 {
		t.Errorf("an un-suspend also closed sockets: %+v", notifier.calls)
	}
}

// The narrowing direction is unchanged and still wins: a patch that leaves the
// grant SUSPENDED closes the socket and announces no widening.
func TestSuspendStillClosesAndNeverWidens(t *testing.T) {
	store := &fakeShareInviteStore{patched: patchedGrantRow(true, true), patchee: shareViewerUser}
	inv := &fakeAccessInvalidator{}
	notifier := &recordingNotifier{inv: inv}
	widener := &recordingWidener{inv: inv}

	rec := doShareRequest(t, newPatchWidenMux(t, store, inv, notifier, widener),
		http.MethodPatch, sharePatchPath, `{"suspended":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(notifier.calls) != 1 || notifier.calls[0].reason != "suspended" {
		t.Errorf("teardowns = %+v, want one 'suspended'", notifier.calls)
	}
	if len(widener.calls) != 0 {
		t.Errorf("a suspension announced a widening: %+v", widener.calls)
	}
}

// allowRides HAS NO WEBSOCKET EFFECT WHATSOEVER — it governs the §7.8 ride
// surface — so a patch that only moves it must not re-handshake anybody. This
// is why the widen branch reads the REQUEST rather than the resulting row:
// `suspended == false` is also true here.
func TestAllowRidesOnlyPatchNeverTouchesASocket(t *testing.T) {
	store := &fakeShareInviteStore{patched: patchedGrantRow(true, false), patchee: shareViewerUser}
	inv := &fakeAccessInvalidator{}
	notifier := &recordingNotifier{inv: inv}
	widener := &recordingWidener{inv: inv}

	rec := doShareRequest(t, newPatchWidenMux(t, store, inv, notifier, widener),
		http.MethodPatch, sharePatchPath, `{"allowRides":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(widener.calls) != 0 || len(notifier.calls) != 0 {
		t.Errorf("an allowRides-only patch disconnected somebody: widen %+v / close %+v",
			widener.calls, notifier.calls)
	}
	// The UNCONDITIONAL cache bust is unchanged — that rule is deliberately not
	// conditional on which field moved.
	if len(inv.busted) != 1 {
		t.Errorf("cache busts = %v, want the grantee's", inv.busted)
	}
}
