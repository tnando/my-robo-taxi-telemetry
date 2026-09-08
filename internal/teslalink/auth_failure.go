package teslalink

import (
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// authFailure classifies a ValidateToken error into the status, wire code and
// message the caller should see.
//
// AN UNANSWERABLE EXISTENCE PROBE IS A 503, NOT A 401 (MYR-612). A 401 tells a
// client its credential is dead, and a phone acts on that by discarding the
// session; a pool wait behind the fail-closed existence check is not grounds
// for that. The refusal itself is unchanged — the request is still refused —
// only the answer's meaning is honest about whether retrying will help. The
// full reasoning is in internal/telemetry/auth_failure.go, which carries the
// same three-line classifier for the REST surfaces there.
func authFailure(err error) (status int, code wserrors.ErrorCode, message string) {
	if auth.IsLookupFailure(err) {
		return http.StatusServiceUnavailable,
			wserrors.ErrCodeServiceUnavailable,
			"authentication is temporarily unavailable; retry shortly"
	}
	return http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token"
}
