package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// stubDriverAccessGate is the MYR-599 consent gate as a test double. Its ZERO
// VALUE OPENS THE GATE, which is the right default for the dozen tests that are
// about something else entirely — but note that the zero value of the REAL
// field is nil, and nil now refuses. The asymmetry is deliberate: a test says
// "assume consent settled" out loud by naming this type, while production
// cannot reach the same state by forgetting a wire.
type stubDriverAccessGate struct {
	pending bool
	err     error
	calls   int
}

func (g *stubDriverAccessGate) PendingDriverAcknowledgmentByVIN(context.Context, string) (bool, error) {
	g.calls++
	if g.err != nil {
		return false, g.err
	}
	return g.pending, nil
}

// serveVINPush drives one POST through the VIN-keyed route with the given
// options, returning the recorder. The fleet server 200s, so anything other
// than a 200 came from a gate.
func serveVINPush(t *testing.T, opts ...FleetConfigOption) *httptest.ResponseRecorder {
	t.Helper()
	const userID = "user-123"
	handler := NewFleetConfigHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleOwner{ownerID: userID},
		validTeslaToken(),
		newTestFleetClient(stubFleetServer(t, http.StatusOK, `{"response":{"updated_vehicles":1}}`).URL),
		EndpointConfig{Hostname: "telemetry.example.com", Port: 443},
		discardLogger(),
		opts...,
	)
	mux := http.NewServeMux()
	mux.Handle("POST /api/fleet-config/{vin}", handler)

	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "/api/fleet-config/5YJ3E1EA1PF000001", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// MYR-599 REVIEW FINDING I: THE VIN-KEYED CONSENT GATE IS NOT OPTIONAL.
//
// It used to proceed with a WARN when unwired, on the reasoning that an absent
// dependency is a dev/test configuration and the route a real client reaches
// gates on its own row anyway. Both halves were true and the conclusion was
// still wrong — the failure an optional consent check permits is not a quieter
// card, it is a telemetry config pushed at a third party's vehicle. An unwired
// gate is a deployment defect, and the honest answer to one is the same refusal
// an unreadable gate earns.
func TestVINKeyedPushRefusesWithoutADriverAccessGate(t *testing.T) {
	rec := serveVINPush(t)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — an unwired consent gate must refuse, not push (body %s)",
			rec.Code, rec.Body.String())
	}
	var env wserrors.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if env.Error.Code == "" {
		t.Error("no error code; the client cannot tell this from a success")
	}
}

// TestVINKeyedPushGateOutcomes walks the three answers the wired gate can give.
// The 409 and the 503 are different on purpose: one is a state the caller can
// leave via §7.29, the other is the server admitting it could not tell.
func TestVINKeyedPushGateOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		gate     *stubDriverAccessGate
		wantCode int
		because  string
	}{
		{
			name:     "a settled gate pushes",
			gate:     &stubDriverAccessGate{},
			wantCode: http.StatusOK,
		},
		{
			name:     "an unacknowledged driver car is refused",
			gate:     &stubDriverAccessGate{pending: true},
			wantCode: http.StatusConflict,
			because: "the caller is not forbidden and nothing failed — the request does not " +
				"apply to this vehicle yet, and §7.29 is the specific thing that changes that",
		},
		{
			name:     "a gate that cannot be read refuses rather than guessing",
			gate:     &stubDriverAccessGate{err: errors.New("db down")},
			wantCode: http.StatusServiceUnavailable,
			because: "every other 'we could not tell' in this package costs a quieter card; " +
				"this one would spend somebody else's consent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveVINPush(t, WithDriverAccessGate(tt.gate))

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d (%s) — body %s",
					rec.Code, tt.wantCode, tt.because, rec.Body.String())
			}
			if tt.gate.calls != 1 {
				t.Errorf("gate calls = %d, want 1", tt.gate.calls)
			}
		})
	}
}
