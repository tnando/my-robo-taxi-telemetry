package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// MYR-609 — POST /api/vehicles/{vehicleId}/share/extend.
//
// The endpoint is the only one on this surface that produces an ACCEPTED grant
// with no redemption, so most of what is asserted here is what it REFUSES: a
// caller who is not the owner, a source share that is not theirs, a source that
// was never accepted, and a person who already has the car.

const shareExtendPath = "/api/vehicles/" + shareFixtureVeh + "/share/extend"

// extendedGrantRow is the row the store returns from a successful extend: an
// ACCEPTED grant on the path vehicle, carrying the source's label and flags and
// no code (the store blanks it in SQL for any non-pending row).
func extendedGrantRow() ShareInviteRow {
	acceptedAt := time.Date(2026, 9, 7, 18, 30, 0, 0, time.UTC)
	name := "Sam"
	return ShareInviteRow{
		ID:         "cshx0123456789abcdef0123456789ab",
		VehicleID:  shareFixtureVeh,
		Label:      "Mira Chen",
		Permission: "rides",
		Grant:      auth.ShareGrant{AllowRides: true},
		Status:     shareStatusAccepted,
		CreatedAt:  acceptedAt,
		AcceptedAt: &acceptedAt,

		AcceptedByName: &name,
	}
}

func TestShareExtendHandler_OwnerExtendsAnAcceptedGrant(t *testing.T) {
	store := &fakeShareInviteStore{extended: extendedGrantRow(), extendee: shareViewerUser}
	invalidator := &fakeAccessInvalidator{}
	mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, invalidator)

	rec := doShareRequest(t, mux, http.MethodPost, shareExtendPath,
		`{"shareId":"csh0123456789abcdef0123456789abcd"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	// The store is asked for exactly what the request named, owner-scoped from
	// the TOKEN and vehicle-scoped from the PATH — neither is client-supplied.
	want := ShareExtendInput{
		OwnerUserID:     shareOwnerUser,
		TargetVehicleID: shareFixtureVeh,
		SourceShareID:   "csh0123456789abcdef0123456789abcd",
	}
	if store.extendedAs != want {
		t.Errorf("store received %+v, want %+v", store.extendedAs, want)
	}

	body := decodeShareBody(t, rec)
	if body["status"] != shareStatusAccepted {
		t.Errorf("status = %v, want accepted — an extended grant is live immediately", body["status"])
	}
	if body["vehicleId"] != shareFixtureVeh {
		t.Errorf("vehicleId = %v, want the PATH vehicle %s", body["vehicleId"], shareFixtureVeh)
	}
	// The flags exist because the row is accepted; permission is DERIVED from
	// them (§7.5.0), so a `rides` grant reads back as `rides`.
	if body["allowRides"] != true {
		t.Errorf("allowRides = %v, want true — the source grant's capability is copied", body["allowRides"])
	}
	if body["suspended"] != false {
		t.Errorf("suspended = %v, want false", body["suspended"])
	}
	if body["permission"] != "rides" {
		t.Errorf("permission = %v, want rides", body["permission"])
	}
	// An accepted row carries neither credential, and there is no pending
	// branch for a share link to be minted in.
	if _, ok := body["code"]; ok {
		t.Error("an accepted row must never carry `code` — it is a live bearer credential")
	}
	if _, ok := body["shareUrl"]; ok {
		t.Error("an accepted row must never carry `shareUrl` — it wraps the code")
	}
	if _, ok := body["expiresAt"]; ok {
		t.Error("an accepted grant does not expire, so `expiresAt` must be omitted")
	}

	// THE GRANTEE's cache is busted, not the owner's: the person whose access
	// set widened is the one whose next handshake must see the new car.
	if len(invalidator.busted) != 1 || invalidator.busted[0] != shareViewerUser {
		t.Errorf("busted = %v, want exactly [%s] — the grantee, not the owner",
			invalidator.busted, shareViewerUser)
	}
}

func TestShareExtendHandler_Refusals(t *testing.T) {
	const validBody = `{"shareId":"csh0123456789abcdef0123456789abcd"}`

	t.Run("a person who already has the car is 409 already_shared", func(t *testing.T) {
		store := &fakeShareInviteStore{extendErr: ErrShareAlreadyGranted}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, shareExtendPath, validBody)

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
		}
		var env wserrors.ErrorEnvelope
		if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if env.Error.Code != wserrors.ErrCodeConflict {
			t.Errorf("code = %q, want conflict", env.Error.Code)
		}
		if env.Error.SubCode == nil || *env.Error.SubCode != string(wserrors.SubCodeAlreadyShared) {
			t.Errorf("subCode = %v, want already_shared — a client branches on it to mark that "+
				"person as already having the car rather than retrying", env.Error.SubCode)
		}
	})

	// The store collapses missing / foreign / pending / revoked into ONE
	// sdk.ErrNotFound, and the handler must not re-separate them: a caller who
	// could tell them apart would have an oracle for other owners' invite ids.
	t.Run("a source share that is not extendable is 404, indistinguishably", func(t *testing.T) {
		for _, name := range []string{
			"another owner's accepted grant",
			"a pending invite nobody has redeemed",
			"a revoked tombstone",
			"an id that never existed",
		} {
			t.Run(name, func(t *testing.T) {
				store := &fakeShareInviteStore{extendErr: sdk.ErrNotFound}
				mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

				rec := doShareRequest(t, mux, http.MethodPost, shareExtendPath, validBody)

				if rec.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
				}
				if got := rec.Body.String(); !jsonHasErrorCode(t, got, wserrors.ErrCodeNotFound) {
					t.Errorf("body = %s, want error.code not_found", got)
				}
			})
		}
	})

	t.Run("a caller who does not own the path vehicle is 403 and never reaches the store", func(t *testing.T) {
		store := &fakeShareInviteStore{extended: extendedGrantRow()}
		mux := newShareInviteMux(t, shareViewerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, shareExtendPath, validBody)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 vehicle_not_owned (body %s)", rec.Code, rec.Body.String())
		}
		if store.extendCalled {
			t.Error("the store was reached by a caller who does not own the vehicle")
		}
	})

	t.Run("no bearer token is 401", func(t *testing.T) {
		store := &fakeShareInviteStore{extended: extendedGrantRow()}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, shareExtendPath,
			bytes.NewReader([]byte(validBody)))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
		}
		if store.extendCalled {
			t.Error("the store was reached without a token")
		}
	})

	t.Run("a target vehicle the store says is not ours is 403", func(t *testing.T) {
		// Unreachable behind authOwner today; asserted so the mapping is a
		// decision rather than a fall into the 500 default.
		store := &fakeShareInviteStore{extendErr: ErrShareVehicleNotOwned}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, shareExtendPath, validBody)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unclassified store failure is 500 and busts nothing", func(t *testing.T) {
		store := &fakeShareInviteStore{extendErr: errors.New("boom")}
		invalidator := &fakeAccessInvalidator{}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, invalidator)

		rec := doShareRequest(t, mux, http.MethodPost, shareExtendPath, validBody)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
		}
		if len(invalidator.busted) != 0 {
			t.Errorf("busted = %v, want none — no grant was created", invalidator.busted)
		}
	})
}

// The body has exactly one required field, so every malformed shape is a client
// bug and answers 400 — never the 404 reserved for "that share is not
// extendable by you", whose indistinguishability across four causes is the
// property this keeps intact.
func TestShareExtendHandler_BodyIsStrict(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not JSON", `{`},
		{"empty body", ``},
		{"missing shareId", `{}`},
		{"blank shareId", `{"shareId":""}`},
		{"whitespace-only shareId", `{"shareId":"   "}`},
		{"an unknown key", `{"shareId":"csh0123456789abcdef0123456789abcd","permission":"rides"}`},
		{"shareId of the wrong type", `{"shareId":42}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeShareInviteStore{extended: extendedGrantRow()}
			mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

			rec := doShareRequest(t, mux, http.MethodPost, shareExtendPath, tt.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if store.extendCalled {
				t.Error("a malformed request reached the store")
			}
		})
	}
}

// The MYR-599 consent gate covers this route for the same reason it covers
// create: extending a grant hands a third party standing access to a car whose
// Tesla owner is not yet on record as agreeing it belongs here. A route that
// skipped it would be a way around §7.29 that create closes.
func TestShareExtendHandler_RefusesUnacknowledgedDriverCar(t *testing.T) {
	newMux := func(t *testing.T, access VehicleDriverAccess, store ShareInviteStore) *http.ServeMux {
		t.Helper()
		row := fixtureSnapshotRow(shareOwnerUser)
		row.DriverAccess = access
		h := NewShareInviteHandler(
			&stubTokenValidator{userID: shareOwnerUser},
			&stubVehicleSnapshotReader{row: row},
			store,
			nil,
			testShareLinkSigner(t),
			discardLogger(),
		)
		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/vehicles/{vehicleId}/share/extend", h.ServeExtend)
		return mux
	}
	const body = `{"shareId":"csh0123456789abcdef0123456789abcd"}`

	t.Run("refused with a 409 and nothing written", func(t *testing.T) {
		store := &fakeShareInviteStore{extended: extendedGrantRow()}
		rec := doShareRequest(t, newMux(t, VehicleDriverAccess{Present: true}, store),
			http.MethodPost, shareExtendPath, body)

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
		}
		if store.extendCalled {
			t.Error("a grant was extended onto a car whose owner has not been recorded as agreeing it belongs here")
		}
	})

	t.Run("an acknowledged driver car may extend", func(t *testing.T) {
		store := &fakeShareInviteStore{extended: extendedGrantRow(), extendee: shareViewerUser}
		acked := VehicleDriverAccess{Present: true, AcknowledgedAt: time.Now().Add(-time.Hour)}
		rec := doShareRequest(t, newMux(t, acked, store), http.MethodPost, shareExtendPath, body)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 — the gate is the acknowledgment, not the access type (body %s)",
				rec.Code, rec.Body.String())
		}
	})
}

// jsonHasErrorCode reports whether a REST error envelope carries the given code.
func jsonHasErrorCode(t *testing.T, body string, want wserrors.ErrorCode) bool {
	t.Helper()
	var env wserrors.ErrorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope: %v (raw %s)", err, body)
	}
	return env.Error.Code == want
}
