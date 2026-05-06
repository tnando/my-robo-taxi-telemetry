package ws

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestHub_OriginRestriction verifies the NFR-3.22 origin allow-list is
// enforced at the WebSocket handler. The coder/websocket library does
// the matching; this test pins the contract end-to-end so a future
// refactor cannot silently weaken it (MYR-17).
//
// Matching semantics (per coder/websocket authenticateOrigin):
//   - empty Origin header: accepted (same-origin or non-browser client).
//   - Origin host == request host: accepted (same-origin).
//   - pattern with "://": matched against scheme+host.
//   - pattern without "://": matched against host only (admits any scheme).
//   - cross-origin without a matching pattern: HTTP 403.
func TestHub_OriginRestriction(t *testing.T) {
	tests := []struct {
		name           string
		patterns       []string
		origin         string
		wantStatusCode int
	}{
		{
			name:           "exact production origin admitted",
			patterns:       []string{"https://myrobotaxi.app", "https://www.myrobotaxi.app"},
			origin:         "https://myrobotaxi.app",
			wantStatusCode: http.StatusSwitchingProtocols,
		},
		{
			name:           "production www origin admitted",
			patterns:       []string{"https://myrobotaxi.app", "https://www.myrobotaxi.app"},
			origin:         "https://www.myrobotaxi.app",
			wantStatusCode: http.StatusSwitchingProtocols,
		},
		{
			name:           "scheme mismatch rejected",
			patterns:       []string{"https://myrobotaxi.app"},
			origin:         "http://myrobotaxi.app",
			wantStatusCode: http.StatusForbidden,
		},
		{
			name:           "evil host rejected",
			patterns:       []string{"https://myrobotaxi.app"},
			origin:         "https://evil.example.com",
			wantStatusCode: http.StatusForbidden,
		},
		{
			name:           "subdomain not implicitly admitted by exact pattern",
			patterns:       []string{"https://myrobotaxi.app"},
			origin:         "https://staging.myrobotaxi.app",
			wantStatusCode: http.StatusForbidden,
		},
		{
			name:           "wildcard subdomain admitted by glob pattern",
			patterns:       []string{"https://*.myrobotaxi.app"},
			origin:         "https://staging.myrobotaxi.app",
			wantStatusCode: http.StatusSwitchingProtocols,
		},
		{
			name:           "localhost dev pattern admits any port",
			patterns:       []string{"localhost:*"},
			origin:         "http://localhost:3000",
			wantStatusCode: http.StatusSwitchingProtocols,
		},
		{
			name:           "127.0.0.1 dev pattern admits any port",
			patterns:       []string{"127.0.0.1:*"},
			origin:         "http://127.0.0.1:8080",
			wantStatusCode: http.StatusSwitchingProtocols,
		},
		{
			name:           "empty Origin header accepted (same-origin / non-browser)",
			patterns:       []string{"https://myrobotaxi.app"},
			origin:         "",
			wantStatusCode: http.StatusSwitchingProtocols,
		},
		{
			name:           "fail-closed: no patterns rejects cross-origin",
			patterns:       nil,
			origin:         "https://evil.example.com",
			wantStatusCode: http.StatusForbidden,
		},
		{
			name:           "fail-closed: no patterns still admits empty Origin",
			patterns:       nil,
			origin:         "",
			wantStatusCode: http.StatusSwitchingProtocols,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newTestHub(t)
			t.Cleanup(hub.Stop)

			auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
			handler := hub.Handler(auth, HandlerConfig{
				OriginPatterns: tt.patterns,
				AuthTimeout:    200 * time.Millisecond,
				WriteTimeout:   500 * time.Millisecond,
			})
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/ws", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			// Synthetic WS upgrade headers so the request reaches
			// websocket.Accept's origin check; we never finish the
			// handshake (no need — the origin check runs first).
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatusCode {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status: got %d, want %d (body: %q)", resp.StatusCode, tt.wantStatusCode, body)
			}

			// On rejection, verify the body carries a clear reason so
			// an operator chasing "why is my browser blocked?" sees the
			// failure inline rather than only in server logs.
			if tt.wantStatusCode == http.StatusForbidden {
				body, _ := io.ReadAll(resp.Body)
				if !strings.Contains(strings.ToLower(string(body)), "origin") {
					t.Errorf("403 body should mention origin; got %q", body)
				}
			}
		})
	}
}

// TestHub_OriginRestriction_FullDial walks one allowed origin all the
// way through the handshake to prove origin acceptance does not break
// the auth flow. The other cases above stop at the upgrade response.
func TestHub_OriginRestriction_FullDial(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	handler := hub.Handler(auth, HandlerConfig{
		OriginPatterns: []string{"https://myrobotaxi.app"},
		AuthTimeout:    500 * time.Millisecond,
		WriteTimeout:   500 * time.Millisecond,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/ws"
	conn, dialResp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://myrobotaxi.app"}},
	})
	if err != nil {
		t.Fatalf("dial allowed origin: %v", err)
	}
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	authMsg := mustMarshalTest(t, wsMessage{
		Type:    msgTypeAuth,
		Payload: mustMarshalRaw(t, authPayload{Token: "token"}),
	})
	if err := conn.Write(ctx, websocket.MessageText, authMsg); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read auth_ok: %v", err)
	}
}
