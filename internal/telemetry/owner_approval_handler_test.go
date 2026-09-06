package telemetry

// §7.29 POST /api/tesla/vehicles/{vehicleId}/acknowledge-owner-approval.
//
// The tests are organised around the fact that this endpoint's job is to WRITE
// A CONSENT RECORD, so every case is framed as the thing that would be wrong
// about the record — or about who could produce one — if the rule it pins were
// removed.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// fakeApprovalRecorder stands in for *store.VehicleRepo. It records what it was
// asked to write, which is the point: the assertions are about the CONTENT of
// the consent record, not merely that a call happened.
type fakeApprovalRecorder struct {
	calls     int
	vehicleID string
	userID    string
	version   string
	at        time.Time
	// recorded is what the store reports: true when an UNACKNOWLEDGED driver
	// row was stamped, false for an owner-access car and for a repeat.
	recorded bool
	err      error
}

func (f *fakeApprovalRecorder) AcknowledgeOwnerApproval(
	_ context.Context, vehicleID, userID, version string, now time.Time,
) (bool, error) {
	f.calls++
	f.vehicleID, f.userID, f.version, f.at = vehicleID, userID, version, now
	return f.recorded, f.err
}

// driverRow is setupRow with an UNACKNOWLEDGED driver-access row on it.
func driverRow(mutate func(*VehicleSnapshotRow)) VehicleSnapshotRow {
	return setupRow(func(r *VehicleSnapshotRow) {
		r.DriverAccess = VehicleDriverAccess{
			Present:   true,
			CreatedAt: setupNow.Add(-2 * time.Hour),
		}
		if mutate != nil {
			mutate(r)
		}
	})
}

func newApprovalHandler(
	reader *fakeSnapshotReader, rec *fakeApprovalRecorder, completer setupCompleter,
) *OwnerApprovalHandler {
	return NewOwnerApprovalHandler(
		&stubTokenValidator{userID: "user-1"}, reader, rec, completer, discardLogger())
}

func approvalRequest(vehicleID, body string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/tesla/vehicles/"+vehicleID+"/acknowledge-owner-approval", strings.NewReader(body))
	r.SetPathValue("vehicleId", vehicleID)
	r.Header.Set("Authorization", "Bearer session-jwt")
	return r
}

const validAckBody = `{"acknowledgmentVersion":"owner-approval-v1"}`

// THE HAPPY PATH, and the two halves that must both hold: the record says the
// right thing, and the push that follows it actually runs.
func TestOwnerApprovalHandlerRecordsThenPushes(t *testing.T) {
	// Two rows: the pre-stamp read the handler authorizes against, then the
	// POST-STAMP re-read. The second has the gate CLEARED, which is what the
	// real GetByID would return after the write.
	acked := driverRow(func(r *VehicleSnapshotRow) {
		r.DriverAccess.AcknowledgedAt = setupNow
	})
	reader := &fakeSnapshotReader{rows: []VehicleSnapshotRow{driverRow(nil), acked}}
	rec := &fakeApprovalRecorder{recorded: true}
	comp := &fakeCompleter{state: SetupState{
		State: SetupStateAwaitingVirtualKey, Since: "2026-08-09T04:00:00Z",
	}}

	w := httptest.NewRecorder()
	newApprovalHandler(reader, rec, comp).ServeHTTP(w, approvalRequest("veh-1", validAckBody))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if rec.calls != 1 {
		t.Fatalf("recorder calls = %d, want 1", rec.calls)
	}
	// The record names the CAR, the PERSON and the COPY VERSION — the three
	// things an owner-side complaint would be answered with.
	if rec.vehicleID != "veh-1" || rec.userID != "user-1" || rec.version != "owner-approval-v1" {
		t.Errorf("recorded (%s, %s, %s), want (veh-1, user-1, owner-approval-v1)",
			rec.vehicleID, rec.userID, rec.version)
	}
	if rec.at.IsZero() {
		t.Error("recorded instant is zero — a consent record with no time is not a record")
	}
	// The SAME best-effort push complete-setup performs.
	if got := comp.calls.Load(); got != 1 {
		t.Errorf("completer calls = %d, want 1 — the acknowledgment must be followed by the push", got)
	}

	var body setupCompletionResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.VehicleID != "veh-1" || body.SetupState.State != SetupStateAwaitingVirtualKey {
		t.Errorf("body = %+v, want veh-1 / awaiting_virtual_key", body)
	}
}

// THE BUG THIS CATCHES IS THE ENDPOINT REFUSING ITS OWN EFFECT. The row the
// handler authorized against was read BEFORE the stamp, so its DriverAccess
// still says "unacknowledged". Hand that stale row to the completer and the
// consent gate inside it fires — answering `awaiting_owner_acknowledgment` to
// the very call that just satisfied the acknowledgment, leaving a client
// looping on a sheet it has already confirmed.
func TestOwnerApprovalHandlerRereadsTheRowBeforePushing(t *testing.T) {
	acked := driverRow(func(r *VehicleSnapshotRow) {
		r.DriverAccess.AcknowledgedAt = setupNow
	})
	reader := &fakeSnapshotReader{rows: []VehicleSnapshotRow{driverRow(nil), acked}}
	rec := &fakeApprovalRecorder{recorded: true}
	// A REAL completer, not the fake: the gate under test lives inside it.
	comp := NewSetupCompleter(SetupCompleterDeps{
		Tokens: &fakeTokenResolver{err: ErrTeslaTokenUnavailable},
	}, SetupCompletionConfig{}, discardLogger())

	w := httptest.NewRecorder()
	newApprovalHandler(reader, rec, comp).ServeHTTP(w, approvalRequest("veh-1", validAckBody))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var body setupCompletionResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.SetupState.State == SetupStateAwaitingOwnerAcknowledgment {
		t.Fatal("the endpoint answered awaiting_owner_acknowledgment to the call that satisfied it " +
			"— the post-stamp re-read is missing")
	}
	// The reader was consulted TWICE: once to authorize, once after the stamp.
	if reader.calls != 2 {
		t.Errorf("reader calls = %d, want 2 (authorize, then re-read after the stamp)", reader.calls)
	}
}

// AN OWNER-ACCESS CAR IS A 200 NO-OP, and the load-bearing half is the second
// assertion: no audit row is written for a non-event. A client that cannot tell
// the two cases apart is never punished for asking.
//
// The re-read is also skipped here, which is not merely an optimisation: there
// was no write, so there is nothing whose effect could need re-reading.
func TestOwnerApprovalHandlerOwnerAccessCarIsANoOp(t *testing.T) {
	reader := &fakeSnapshotReader{rows: []VehicleSnapshotRow{setupRow(nil)}}
	rec := &fakeApprovalRecorder{recorded: false} // nothing to stamp
	comp := &fakeCompleter{state: SetupState{State: SetupStateConfiguring, Since: "2026-08-09T04:00:00Z"}}

	w := httptest.NewRecorder()
	newApprovalHandler(reader, rec, comp).ServeHTTP(w, approvalRequest("veh-1", validAckBody))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an owner's own car has nothing to acknowledge, which is not an error", w.Code)
	}
	if reader.calls != 1 {
		t.Errorf("reader calls = %d, want 1 (nothing was written, so nothing needs re-reading)", reader.calls)
	}
	if got := comp.calls.Load(); got != 1 {
		t.Errorf("completer calls = %d, want 1 — the push still runs", got)
	}
}

// IDEMPOTENCE. A repeat on an already-acknowledged car is a 200 and NEVER a
// 409: it is the safe retry for a client that lost the first response, and the
// usable "try again" for a push that failed transiently. The store reports
// recorded=false because its `AND acknowledged_at IS NULL` predicate excluded
// the row — first acknowledgment wins, and a later call cannot re-date it.
func TestOwnerApprovalHandlerRepeatIsA200(t *testing.T) {
	already := driverRow(func(r *VehicleSnapshotRow) {
		r.DriverAccess.AcknowledgedAt = setupNow.Add(-time.Hour)
	})
	reader := &fakeSnapshotReader{rows: []VehicleSnapshotRow{already}}
	rec := &fakeApprovalRecorder{recorded: false}
	comp := &fakeCompleter{state: SetupState{State: SetupStateConfiguring, Since: "2026-08-09T04:00:00Z"}}

	w := httptest.NewRecorder()
	newApprovalHandler(reader, rec, comp).ServeHTTP(w, approvalRequest("veh-1", validAckBody))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a second acknowledgment is a retry, never a conflict", w.Code)
	}
	if got := comp.calls.Load(); got != 1 {
		t.Errorf("completer calls = %d, want 1 — the repeat must still re-run the push", got)
	}
}

// AUTHORIZATION. The divergence worth pinning is that BOTH failures answer 404:
// an endpoint whose job is recording a consent must not double as a way to
// enumerate which vehicleIds exist, so "no such car" and "not your car" are made
// indistinguishable. Its §7.23 sibling answers 403 on the second.
//
// And in every failing case the recorder is never reached — no consent record
// is ever produced for a car the caller cannot show they hold.
func TestOwnerApprovalHandlerAuthorization(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		vehicleID  string
		body       string
		noAuth     bool
		authErr    error
		readerErr  error
		callerID   string
		wantStatus int
		wantCode   wserrors.ErrorCode
	}{
		{
			name: "owner succeeds", vehicleID: "veh-1", callerID: "user-1",
			body: validAckBody, wantStatus: http.StatusOK,
		},
		{
			name: "unknown vehicle is 404", vehicleID: "nope", callerID: "user-1",
			body: validAckBody, readerErr: sdk.ErrNotFound,
			wantStatus: http.StatusNotFound, wantCode: wserrors.ErrCodeNotFound,
		},
		{
			// THE DIVERGENCE. 404, not the 403 §7.23 answers.
			name: "another user's vehicle is 404, NOT 403", vehicleID: "veh-1", callerID: "someone-else",
			body: validAckBody, wantStatus: http.StatusNotFound, wantCode: wserrors.ErrCodeNotFound,
		},
		{
			name: "missing bearer is 401", vehicleID: "veh-1", callerID: "user-1",
			body: validAckBody, noAuth: true,
			wantStatus: http.StatusUnauthorized, wantCode: wserrors.ErrCodeAuthFailed,
		},
		{
			name: "invalid token is 401", vehicleID: "veh-1", callerID: "user-1",
			body: validAckBody, authErr: errors.New("expired"),
			wantStatus: http.StatusUnauthorized, wantCode: wserrors.ErrCodeAuthFailed,
		},
		{
			name: "GET is 405", vehicleID: "veh-1", callerID: "user-1", method: http.MethodGet,
			body: validAckBody, wantStatus: http.StatusMethodNotAllowed, wantCode: wserrors.ErrCodeInvalidRequest,
		},
		{
			name: "missing vehicleId is 400", vehicleID: "", callerID: "user-1",
			body: validAckBody, wantStatus: http.StatusBadRequest, wantCode: wserrors.ErrCodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeSnapshotReader{
				rows: []VehicleSnapshotRow{driverRow(nil), driverRow(func(r *VehicleSnapshotRow) {
					r.DriverAccess.AcknowledgedAt = setupNow
				})},
				err: tt.readerErr,
			}
			rec := &fakeApprovalRecorder{recorded: true}
			h := NewOwnerApprovalHandler(
				&stubTokenValidator{userID: tt.callerID, err: tt.authErr},
				reader, rec,
				&fakeCompleter{state: SetupState{State: SetupStateAwaitingVirtualKey, Since: "2026-08-09T04:00:00Z"}},
				discardLogger())

			r := approvalRequest(tt.vehicleID, tt.body)
			if tt.method != "" {
				r.Method = tt.method
			}
			if tt.noAuth {
				r.Header.Del("Authorization")
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus == http.StatusOK {
				return
			}
			if rec.calls != 0 {
				t.Errorf("recorder was called %d times on a failed request — no consent record may be "+
					"produced for a car the caller cannot show they hold", rec.calls)
			}
			assertErrorCode(t, w, tt.wantCode)
		})
	}
}

// THE BODY. It is the one field of a consent record the client supplies, so it
// is validated on presence and length — and DELIBERATELY NOT against a list of
// known ids, because a client shipped before a copy revision must not be blocked
// from finishing setup, and every copy change would otherwise be a coordinated
// deploy.
func TestOwnerApprovalHandlerBodyValidation(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		// wantVersion, when set, is what must reach the store.
		wantVersion string
	}{
		{
			name: "the canonical id", body: validAckBody,
			wantStatus: http.StatusOK, wantVersion: "owner-approval-v1",
		},
		{
			// THE RULE MOST LIKELY TO BE "TIGHTENED" BY MISTAKE. An id this
			// server has never heard of is still recorded exactly as sent.
			name: "an UNRECOGNISED id is still recorded", body: `{"acknowledgmentVersion":"owner-approval-v99"}`,
			wantStatus: http.StatusOK, wantVersion: "owner-approval-v99",
		},
		{
			name: "surrounding whitespace is trimmed", body: `{"acknowledgmentVersion":"  owner-approval-v1  "}`,
			wantStatus: http.StatusOK, wantVersion: "owner-approval-v1",
		},
		{name: "missing field", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "empty string", body: `{"acknowledgmentVersion":""}`, wantStatus: http.StatusBadRequest},
		{name: "whitespace only", body: `{"acknowledgmentVersion":"   "}`, wantStatus: http.StatusBadRequest},
		{name: "null", body: `{"acknowledgmentVersion":null}`, wantStatus: http.StatusBadRequest},
		{name: "wrong type", body: `{"acknowledgmentVersion":42}`, wantStatus: http.StatusBadRequest},
		{name: "malformed json", body: `{`, wantStatus: http.StatusBadRequest},
		{name: "empty body", body: ``, wantStatus: http.StatusBadRequest},
		{
			// Strict decode. A client sending a field this server does not know
			// believes it matters, and silently dropping it on a CONSENT record
			// is the wrong failure.
			name: "unknown field is refused", body: `{"acknowledgmentVersion":"owner-approval-v1","extra":1}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "over the 64-rune cap",
			body:       `{"acknowledgmentVersion":"` + strings.Repeat("v", 65) + `"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			// RUNES, NOT BYTES: 64 multi-byte characters is within the cap even
			// though it is well over 64 bytes.
			name:        "64 multi-byte runes are within the cap",
			body:        `{"acknowledgmentVersion":"` + strings.Repeat("é", 64) + `"}`,
			wantStatus:  http.StatusOK,
			wantVersion: strings.Repeat("é", 64),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeSnapshotReader{rows: []VehicleSnapshotRow{
				driverRow(nil),
				driverRow(func(r *VehicleSnapshotRow) { r.DriverAccess.AcknowledgedAt = setupNow }),
			}}
			rec := &fakeApprovalRecorder{recorded: true}
			h := newApprovalHandler(reader, rec,
				&fakeCompleter{state: SetupState{State: SetupStateAwaitingVirtualKey, Since: "2026-08-09T04:00:00Z"}})

			w := httptest.NewRecorder()
			h.ServeHTTP(w, approvalRequest("veh-1", tt.body))

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				if rec.calls != 0 {
					t.Errorf("recorder ran %d times on a rejected body — nothing may be stored from one", rec.calls)
				}
				return
			}
			if rec.version != tt.wantVersion {
				t.Errorf("stored version = %q, want %q", rec.version, tt.wantVersion)
			}
		})
	}
}

// A FAILED PUSH IS NOT A FAILED ACKNOWLEDGMENT. The consent was genuinely given
// and is committed; only the Tesla-side step is uncertain. The error is reported
// in §7.23's own vocabulary — the shared mapping — so a client that handles
// complete-setup's errors handles this route's unchanged.
func TestOwnerApprovalHandlerPushFailureKeepsTheAcknowledgment(t *testing.T) {
	reader := &fakeSnapshotReader{rows: []VehicleSnapshotRow{
		driverRow(nil),
		driverRow(func(r *VehicleSnapshotRow) { r.DriverAccess.AcknowledgedAt = setupNow }),
	}}
	rec := &fakeApprovalRecorder{recorded: true}
	comp := &fakeCompleter{err: ErrSetupVehicleUnreachable}

	w := httptest.NewRecorder()
	newApprovalHandler(reader, rec, comp).ServeHTTP(w, approvalRequest("veh-1", validAckBody))

	// The SAME status §7.23 gives for the same sentinel.
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (the shared §7.23 mapping)", w.Code)
	}
	assertErrorCode(t, w, wserrors.ErrCodeVehicleAsleep)
	// The record still happened. A retry is safe precisely because it did.
	if rec.calls != 1 {
		t.Errorf("recorder calls = %d, want 1 — a push failure must not roll back a consent", rec.calls)
	}
}

// A STORE FAILURE IS A 500 AND PUSHES NOTHING. The gate is only opened by a
// committed write, so a write we could not make must not be followed by the
// action it was supposed to authorize.
func TestOwnerApprovalHandlerStoreFailurePushesNothing(t *testing.T) {
	reader := &fakeSnapshotReader{rows: []VehicleSnapshotRow{driverRow(nil)}}
	rec := &fakeApprovalRecorder{err: errors.New("db down")}
	comp := &fakeCompleter{state: SetupState{State: SetupStateConfiguring, Since: "2026-08-09T04:00:00Z"}}

	w := httptest.NewRecorder()
	newApprovalHandler(reader, rec, comp).ServeHTTP(w, approvalRequest("veh-1", validAckBody))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if got := comp.calls.Load(); got != 0 {
		t.Errorf("completer calls = %d, want 0 — nothing may be pushed on an unrecorded consent", got)
	}
}

// MYR-599 REVIEW FINDING H: §7.29 MUST NOT 500 FOR A REQUEST THAT DURABLY
// SUCCEEDED.
//
// The acknowledgment is committed by the time the post-stamp re-read runs — the
// row is stamped, the audit row is written, the gate is open. If that re-read
// fails, a 500 tells the client the request failed when it did not. The client's
// only sensible response to a 500 is to keep showing the acknowledgment sheet
// for a car whose consent is already on record, and to retry into a call that,
// being idempotent, will record nothing and can fail here again.
//
// The honest answer is a 200 carrying the state that can still be derived: the
// row in hand with the acknowledgment applied. It must NEVER be `configuring`,
// because no push ran on this path — the completer is deliberately not invoked
// once the row is unreadable.
func TestOwnerApprovalHandlerAnswers200WhenTheRereadFails(t *testing.T) {
	tests := []struct {
		name      string
		row       VehicleSnapshotRow
		wantState string
		because   string
	}{
		{
			name: "a car seeded awaiting_owner_ack answers owner_access_required",
			row: driverRow(func(r *VehicleSnapshotRow) {
				r.SetupSchedule = VehicleSetupSchedule{
					Present: true, LastOutcome: outcomeAwaitingOwnerAck,
				}
			}),
			wantState: SetupStateOwnerAccessRequired,
			because: "the ordinary case — the seed label makes no claim once the gate is open, " +
				"and Tesla's config POST is owner-only, so a refusal is exactly what the push " +
				"we could not run would have met",
		},
		{
			name: "a pre-existing awaiting_virtual_key schedule is answered as it stands",
			row: driverRow(func(r *VehicleSnapshotRow) {
				r.SetupSchedule = VehicleSetupSchedule{
					Present:       true,
					LastOutcome:   outcomeAwaitingKey,
					LastAttemptAt: setupNow.Add(-time.Hour),
				}
			}),
			wantState: SetupStateAwaitingVirtualKey,
			because: "the schedule's own evidence survives the failed re-read and is a claim " +
				"about a push that really did happen earlier",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Succeeds for the authorize read, fails for the post-stamp re-read.
			reader := &fakeSnapshotReader{
				rows:     []VehicleSnapshotRow{tc.row},
				err:      errors.New("db down"),
				errAfter: 1,
			}
			rec := &fakeApprovalRecorder{recorded: true}
			comp := &fakeCompleter{state: SetupState{State: SetupStateConfiguring}}

			w := httptest.NewRecorder()
			newApprovalHandler(reader, rec, comp).ServeHTTP(w, approvalRequest("veh-1", validAckBody))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — the acknowledgment COMMITTED; reporting a "+
					"failure strands the one piece of copy this feature exists for (body %s)",
					w.Code, w.Body.String())
			}
			var body setupCompletionResponse
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.SetupState.State != tc.wantState {
				t.Errorf("setupState.state = %q, want %q (%s)",
					body.SetupState.State, tc.wantState, tc.because)
			}
			if body.SetupState.State == SetupStateAwaitingOwnerAcknowledgment {
				t.Error("answered awaiting_owner_acknowledgment to the very call that satisfied " +
					"it — the row in hand is pre-stamp, and the acknowledgment must be applied " +
					"to it before deriving anything")
			}
			if body.SetupState.State == SetupStateConfiguring {
				t.Error("answered `configuring` with no fresh row — nothing was pushed on this " +
					"path, so a progress claim is fabricated")
			}
			if got := comp.calls.Load(); got != 0 {
				t.Errorf("completer calls = %d, want 0 — the push paths gate on a row, and the "+
					"row is exactly what we just failed to read", got)
			}
			if body.VehicleID != "veh-1" {
				t.Errorf("vehicleId = %q, want veh-1", body.VehicleID)
			}
		})
	}
}
