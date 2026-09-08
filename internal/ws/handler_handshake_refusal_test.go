package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	authpkg "github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// HOW A HANDSHAKE IS REFUSED (MYR-612 review).
//
// Three properties, and each of them was wrong before:
//
//	ONE FRAME       §2.3 promises a client sees `auth_ok` OR one `error`, never
//	                more than one. Two paths were writing frames.
//	NO P1           §6.3 / CG-DC-2: the `message` field carries no P1 value. The
//	                second frame carried the whole wrapped error chain, user id
//	                included.
//	CLOSE CODE 1013 for the unanswerable existence probe. `service_unavailable`
//	                is deliberately not a member of ErrorPayload.code — the WS
//	                analogue of a 503 is a close code, not a typed frame.

func refusalServer(t *testing.T, a Authenticator) *httptest.Server {
	t.Helper()
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)
	srv := httptest.NewServer(hub.Handler(a, HandlerConfig{
		AuthTimeout:  200 * time.Millisecond,
		WriteTimeout: 2 * time.Second,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHandshakeRefusal_LookupFailureClosesWith1013 pins the orchestrated
// decision: no frame at all, and the documented "try again later" close code.
func TestHandshakeRefusal_LookupFailureClosesWith1013(t *testing.T) {
	srv := refusalServer(t, &testAuth{
		err: fmt.Errorf("auth.ValidateToken: %w: %w",
			ErrInvalidToken, authpkg.ErrUserLookupFailed),
	})

	conn := dialAndAuthRaw(t, srv.URL, "any-token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err == nil {
		t.Fatalf("the server sent a frame (%s); the WS analogue of a 503 is a "+
			"close code and `service_unavailable` is not a member of ErrorPayload.code", data)
	}
	if got := websocket.CloseStatus(err); got != websocket.StatusTryAgainLater {
		t.Errorf("close code = %d, want %d (1013 Try Again Later) — a database "+
			"hiccup must not read as a dead credential", got, websocket.StatusTryAgainLater)
	}
}

// TestHandshakeRefusal_SendsExactlyOneStaticFrame covers the ordinary refusals.
func TestHandshakeRefusal_SendsExactlyOneStaticFrame(t *testing.T) {
	tests := []struct {
		name     string
		auth     Authenticator
		token    string
		wantCode wserrors.ErrorCode
	}{
		{
			name:     "invalid token",
			auth:     &testAuth{err: ErrInvalidToken},
			token:    "bad-token",
			wantCode: wserrors.ErrCodeAuthFailed,
		},
		{
			// GetUserVehicles failing used to be the OTHER frame writer, with
			// its own message; it is one code and one emission point now.
			name:     "vehicle set could not be loaded",
			auth:     &vehiclesFailAuth{},
			token:    "good-token",
			wantCode: wserrors.ErrCodeAuthFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := refusalServer(t, tt.auth)
			conn := dialAndAuthRaw(t, srv.URL, tt.token)
			defer conn.Close(websocket.StatusNormalClosure, "")

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("no error frame: %v", err)
			}
			var msg wsMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if msg.Type != msgTypeError {
				t.Fatalf("frame type = %q, want %q", msg.Type, msgTypeError)
			}
			var payload errorPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if payload.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", payload.Code, tt.wantCode)
			}
			// NO INTERNAL ERROR TEXT ON THE WIRE. The chain that leaked here
			// carried the resolved user id, which is P1 (§6.3, CG-DC-2).
			for _, leak := range []string{"hub.authenticateClient", "user=", "user-1"} {
				if strings.Contains(payload.Message, leak) {
					t.Errorf("message %q carries the internal error chain (%q)", payload.Message, leak)
				}
			}

			// AND EXACTLY ONE FRAME: the next read must be the close, not a
			// second error.
			_, second, err := conn.Read(ctx)
			if err == nil {
				t.Errorf("a second frame followed the error frame: %s", second)
			}
			if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
				t.Errorf("close code = %d, want %d", got, websocket.StatusPolicyViolation)
			}
		})
	}
}

// vehiclesFailAuth validates the token and then fails the vehicle read — the
// second of the two paths that used to write its own frame.
type vehiclesFailAuth struct{ testAuth }

func (a *vehiclesFailAuth) ValidateToken(_ context.Context, _ string) (string, error) {
	return "user-1", nil
}

func (a *vehiclesFailAuth) GetUserVehicles(_ context.Context, userID string) ([]string, error) {
	return nil, fmt.Errorf("vehicles(user=%s): %w", userID, errors.New("pool timeout"))
}
