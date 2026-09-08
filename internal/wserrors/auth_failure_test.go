package wserrors

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// TestAuthFailure_ClassifiesTheTwoRefusals is the rule itself: a dead
// credential and an unanswerable existence probe are not the same answer.
func TestAuthFailure_ClassifiesTheTwoRefusals(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   ErrorCode
	}{
		{
			name:       "a plainly bad token",
			err:        errors.New("token signature is invalid"),
			wantStatus: http.StatusUnauthorized,
			wantCode:   ErrCodeAuthFailed,
		},
		{
			name:       "the account really has no row",
			err:        fmt.Errorf("auth.ValidateToken: %w", errors.New("user not found")),
			wantStatus: http.StatusUnauthorized,
			wantCode:   ErrCodeAuthFailed,
		},
		{
			name:       "the probe could not be answered",
			err:        auth.ErrUserLookupFailed,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   ErrCodeServiceUnavailable,
		},
		{
			// THE DEPTH MATTERS. ValidateToken wraps the sentinel behind two
			// further errors, and a classifier using == rather than errors.Is
			// would answer 401 to every real occurrence of this condition.
			name: "the sentinel arrives wrapped, as it does in production",
			err: fmt.Errorf("auth.ValidateToken: %w: %w: %w",
				errors.New("invalid token"), auth.ErrUserLookupFailed,
				errors.New("timeout: context deadline exceeded")),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   ErrCodeServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, message := AuthFailure(tt.err)
			if status != tt.wantStatus || code != tt.wantCode {
				t.Errorf("AuthFailure() = (%d, %q), want (%d, %q)",
					status, code, tt.wantStatus, tt.wantCode)
			}
			// §6.3 / CG-DC-2: the message is static and carries nothing from
			// the error chain, which is where a user id would come from.
			if strings.Contains(message, "ValidateToken") || strings.Contains(message, "user") {
				t.Errorf("message %q echoes the internal error chain", message)
			}
		})
	}
}

// authFailureExemptions are the call sites that legitimately do NOT reach
// wserrors.AuthFailure, each with the reason it cannot.
var authFailureExemptions = map[string]string{
	// The WebSocket has no HTTP status to return and `service_unavailable` is
	// deliberately not a member of ErrorPayload.code — its analogue of the 503
	// arm is close code 1013 (websocket-protocol.md §2.4, §6.2). It makes the
	// SAME distinction, in refuseHandshake, over the same auth.IsLookupFailure.
	"internal/ws/handler.go": "close code 1013, not an HTTP status — see refuseHandshake",
	// The implementation of the check itself.
	"internal/auth": "defines ValidateToken and the ErrUserLookupFailed sentinel",
}

// TestEveryValidateTokenCallSiteClassifiesItsFailure is the GREP-PROOF half,
// and it exists because the bug it guards was a handler that simply forgot.
//
// MYR-612 changed thirty-odd REST surfaces to answer 503 rather than 401 when
// the fail-closed existence probe could not be ANSWERED. One of them —
// vehicle_fleet_config_handler.go — kept its hard-coded 401 through the whole
// change, because nothing in the build could tell that it had. A behavioural
// test per handler would not have caught it either: the one that was missed is
// exactly the one nobody wrote a test for. So this walks the SOURCE and asserts
// the shape, which is a property the next handler cannot opt out of by being
// new.
func TestEveryValidateTokenCallSiteClassifiesItsFailure(t *testing.T) {
	const lookahead = 15
	root := filepath.Join("..")

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(filepath.Join("internal", strings.TrimPrefix(filepath.ToSlash(path), "../")))
		for prefix := range authFailureExemptions {
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return nil
			}
		}

		//nolint:gosec // G122/G304: reading this repository's own source tree, from a test.
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if !strings.Contains(line, ".ValidateToken(") {
				continue
			}
			window := strings.Join(lines[i:min(i+lookahead, len(lines))], "\n")
			if strings.Contains(window, "AuthFailure(") {
				continue
			}
			t.Errorf("%s:%d calls ValidateToken but does not classify the error with "+
				"wserrors.AuthFailure within %d lines.\n"+
				"An unanswerable user-existence probe must answer 503 service_unavailable, "+
				"never 401 auth_failed: a 401 tells a phone its credential is dead and the "+
				"app discards the session (MYR-612, rest-api.md §3.2.1). If this site "+
				"genuinely cannot use the helper, add it to authFailureExemptions with the "+
				"reason.", rel, i+1, lookahead)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
