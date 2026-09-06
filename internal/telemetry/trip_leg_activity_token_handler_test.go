package telemetry

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// §7.21.7 — the LEG anchor of §7.21's per-Activity path,
// `POST`/`DELETE /api/trip-legs/{legId}/activity-token`.
//
// The routes exist because push-to-start is only half a mechanism without them:
// §7.30.8 registers the token that lets the server CREATE a card, ActivityKit
// then hands the app a per-Activity token addressing that one running card, and
// with nowhere to file it the server could raise a leg's card and never update
// or end it.

const legTestID = "cleg_01j9x8h2k4m6n8p0q2r4s6t9"

// legTokenPath carries NO TRIP ID, deliberately: a leg belongs to exactly one
// trip, so the authorization is resolved from the leg rather than restated by
// the client.
func legTokenPath() string {
	return "/api/trip-legs/" + legTestID + "/activity-token"
}

// TestLegActivityTokenRegisters is the happy path, and it pins the two things
// the store needs that the URL carries: the TRIP (which scopes the leg) and the
// LEG (which is the anchor).
func TestLegActivityTokenRegisters(t *testing.T) {
	const token = "8f3a91c0deadbeefcafef00dfeedface" //nolint:gosec // G101: a fixed test fixture, not a credential.

	store := &fakeTripStore{
		trip:            fixtureTrip(),
		legAccessTripID: tripTestID,
		legAccessOpen:   true,
	}
	handler := newTripTestHandler(t, store, true)

	rec := tripRequest(t, handler, http.MethodPost, legTokenPath(),
		`{"activityToken":"`+token+`","sandbox":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	if store.legTokenCalls != 1 {
		t.Fatalf("store saw %d leg registrations, want 1", store.legTokenCalls)
	}
	if store.lastLegTripID != tripTestID || store.lastLegID != legTestID {
		t.Errorf("store got trip=%q leg=%q, want %q/%q — the trip id must come from the "+
			"ACCESS PROBE, never from the caller, and the write must re-assert it",
			store.lastLegTripID, store.lastLegID, tripTestID, legTestID)
	}
	if store.lastLegToken != token || !store.lastLegSandbox {
		t.Errorf("store got token=%q sandbox=%v, want the body's values",
			store.lastLegToken, store.lastLegSandbox)
	}

	// THE TOKEN IS P1 AND IS NEVER ECHOED, exactly as §7.21.1's response rule
	// requires: the caller already knows what it sent, and echoing puts it in
	// every client log and proxy trace.
	if strings.Contains(rec.Body.String(), token) {
		t.Errorf("the response echoed the P1 activity token: %s", rec.Body.String())
	}
	var body activityTokenResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Registered || !body.Sandbox {
		t.Errorf("response = %+v, want {registered:true, sandbox:true}", body)
	}
}

// TestLegActivityTokenRequiresMembership. Registering is a grant of permission
// to write to this phone's lock screen about this trip, so it is gated on the
// trip read — and the refusal is 404, so the endpoint is not an oracle for trip
// ids either.
func TestLegActivityTokenRequiresMembership(t *testing.T) {
	// The probe refuses: unknown leg, or a leg on somebody else's trip. ONE
	// answer for both, so the endpoint cannot be used to discover leg ids.
	store := &fakeTripStore{legAccessErr: ErrTripNotFound}
	handler := newTripTestHandler(t, store, true)

	rec := tripRequest(t, handler, http.MethodPost, legTokenPath(),
		`{"activityToken":"abc123"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. Body: %s", rec.Code, rec.Body.String())
	}
	if store.legTokenCalls != 0 {
		t.Errorf("the store was written to for a caller who is not on the trip")
	}
}

// TestLegActivityTokenRefusesAClosedLeg is the OTHER refusal, and the two must
// stay distinguishable.
//
// A stranger gets 404 and stops. A genuine MEMBER whose leg has ended gets 409
// and ends the card locally — they hold a real card for a real leg of a real
// trip, and telling them it does not exist would be false. Collapsing the two
// would either confirm a guessed leg id to a stranger or make a legitimate
// refusal unreadable.
//
// Both arms are covered: the probe reporting a closed leg, and the WRITE
// refusing after a probe that said open — the race the SQL guard exists for.
func TestLegActivityTokenRefusesAClosedLeg(t *testing.T) {
	tests := []struct {
		name  string
		store *fakeTripStore
	}{
		{
			name:  "the probe reports the leg closed",
			store: &fakeTripStore{legAccessTripID: tripTestID, legAccessOpen: false},
		},
		{
			name: "the leg closes between the probe and the write",
			store: &fakeTripStore{
				legAccessTripID: tripTestID, legAccessOpen: true,
				legRegisterErr: ErrLiveActivityClosed,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newTripTestHandler(t, tt.store, true)
			rec := tripRequest(t, handler, http.MethodPost, legTokenPath(),
				`{"activityToken":"abc123"}`)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409. Body: %s", rec.Code, rec.Body.String())
			}
			assertNoLegSubCode(t, rec)
		})
	}
}

// assertNoLegSubCode pins that the 409 carries no sub-code, matching §7.21.1's
// own refusal: the client's action is the same whichever half of the guard
// fired — end the Activity locally.
func assertNoLegSubCode(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if sub := decodeTripError(t, rec).SubCode; sub != nil && *sub != "" {
		t.Errorf("subCode = %q, want none", *sub)
	}
}

func TestLegActivityTokenRefusalCarriesNoSubCode(t *testing.T) {
	store := &fakeTripStore{legAccessTripID: tripTestID, legAccessOpen: false}
	handler := newTripTestHandler(t, store, true)

	rec := tripRequest(t, handler, http.MethodPost, legTokenPath(),
		`{"activityToken":"abc123"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	assertNoLegSubCode(t, rec)
}

// TestLegActivityTokenValidatesTheToken. The bound and the charset are §7.21.1's
// verbatim, and the charset is a SECURITY control as much as a validation: the
// token is interpolated into the APNs request path, and refusing a shape that
// could not address a device anyway means the pathological input never enters
// the system.
func TestLegActivityTokenValidatesTheToken(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", `{"activityToken":""}`},
		{"non-hex", `{"activityToken":"not-a-token"}`},
		{"too long", `{"activityToken":"` + strings.Repeat("a", maxActivityTokenLen+1) + `"}`},
		{"unknown field", `{"activityToken":"abc123","nope":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeTripStore{legAccessTripID: tripTestID, legAccessOpen: true}
			handler := newTripTestHandler(t, store, true)

			rec := tripRequest(t, handler, http.MethodPost, legTokenPath(), tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400. Body: %s", rec.Code, rec.Body.String())
			}
			if store.legTokenCalls != 0 || store.legAccessCalls != 0 {
				t.Errorf("a refused body still reached the store; the body is validated " +
					"BEFORE the probe, so a malformed request costs no query")
			}
			// The refusal names the FIELD, never the VALUE: an error message is
			// the one place a P1 value most reliably reaches a log without
			// anybody deciding it should.
			if strings.Contains(rec.Body.String(), strings.Repeat("a", 32)) {
				t.Errorf("the refusal echoed the token: %s", rec.Body.String())
			}
		})
	}
}

// TestLegActivityTokenEndIsIdempotentAndUngated is the DELETE's asymmetry with
// the POST, and it is deliberate: this only ever removes the CALLER'S OWN row,
// so the worst a stranger achieves is deleting a row they do not have. A
// participant who has just LEFT must still be able to clear their registration
// — and after leaving they no longer pass the membership read.
func TestLegActivityTokenEndIsIdempotentAndUngated(t *testing.T) {
	store := &fakeTripStore{legAccessErr: ErrTripNotFound, legEnded: false}
	handler := newTripTestHandler(t, store, true)

	rec := tripRequest(t, handler, http.MethodDelete, legTokenPath(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even for a caller the trip read would refuse. Body: %s",
			rec.Code, rec.Body.String())
	}
	if store.legEndCalls != 1 {
		t.Fatalf("store saw %d end calls, want 1", store.legEndCalls)
	}
	if store.legAccessCalls != 0 {
		t.Error("the DELETE ran the access probe; it must not — a participant who has " +
			"just LEFT the trip would then be unable to clear their own registration")
	}
	var body activityEndedResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Ended {
		t.Error("ended = true for a registration that never existed; `false` must cover " +
			"both 'already ended' and 'never registered', indistinguishably")
	}
}

// TestLegActivityTokenEndReportsATransportFailure. 200 is the answer to "there
// was nothing to end", NOT to "the database is down" — telling a client its
// card was deregistered when it was not leaves the server pushing to it.
func TestLegActivityTokenEndReportsATransportFailure(t *testing.T) {
	store := &fakeTripStore{legEndErr: errors.New("connection reset")}
	handler := newTripTestHandler(t, store, true)

	rec := tripRequest(t, handler, http.MethodDelete, legTokenPath(), "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500. Body: %s", rec.Code, rec.Body.String())
	}
}

// TestLegActivityTokenRoutesAnswer503WhenTripsAreOff. The kill switch switches
// the feature off WHOLE, and these two routes are part of it.
func TestLegActivityTokenRoutesAnswer503WhenTripsAreOff(t *testing.T) {
	handler := newTripTestHandler(t, &fakeTripStore{legAccessTripID: tripTestID, legAccessOpen: true}, false)

	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		rec := tripRequest(t, handler, method, legTokenPath(), `{"activityToken":"abc123"}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d, want 503 — a mounted route with the switch off says "+
				"'not right now', where a 404 tells a client to stop asking", method, rec.Code)
		}
	}
}
