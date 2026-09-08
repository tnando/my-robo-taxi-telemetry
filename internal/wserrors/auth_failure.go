package wserrors

import (
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// HOW A REJECTED BEARER TOKEN IS REPORTED (MYR-612).
//
// Every REST surface on this service authenticates the same way — extract the
// bearer, call ValidateToken, refuse on error — and every one of them used to
// answer the SAME 401 for both of the two very different things that error can
// mean:
//
//	the token is bad      the signature, the issuer, the expiry, or the account
//	                      itself is gone. Retrying changes nothing. 401.
//	we could not TELL     the existence probe behind the check could not be
//	                      answered — a pool wait, a statement timeout, a
//	                      cancelled peer sharing the singleflight slot. The
//	                      token may be perfectly good. 503.
//
// COLLAPSING THE SECOND ONTO THE FIRST IS WHAT MYR-612 COST. A participant's
// `POST /api/trips/{tripId}/activity-start-token` was answered 401 at 03:41:55
// on a database hiccup; his next two attempts a minute later were served. A 401
// is the one answer a client is entitled to act on destructively — it means
// "this credential is dead", and the app's response to it is to discard the
// session and send the person back to a sign-in screen. Telling a phone its
// credential is dead because a query queued is a bug with a user-visible worst
// case far larger than the hiccup that caused it.
//
// THE CHECK ITSELF IS UNCHANGED AND STILL FAILS CLOSED: an unanswerable
// existence probe still refuses the request. What changes is only WHICH refusal
// the client is told, and therefore whether retrying is the right response.
//
// ⚠ ONE COPY, HERE, AND THE PLACEMENT IS THE POINT (MYR-612 review). This
// classifier existed in FOUR places — internal/telemetry, internal/push,
// internal/teslalink and, differently, internal/ws — and the fourth had already
// drifted: it answered a typed `service_unavailable` FRAME on a transport whose
// ErrorPayload.code enum deliberately excludes that member. A rule that decides
// what thirty surfaces tell a phone about its credential cannot live in four
// files. It lives beside the error catalog it draws its codes from, which is
// the one package both transports already depend on.

// AuthFailure classifies a ValidateToken error into the status, wire code and
// message the caller should see.
//
// Returned as three values rather than written to the ResponseWriter here
// because every handler has its own `writeError` — the envelope is the
// handler's, the CLASSIFICATION is shared, and that is the only part that must
// not drift between thirty surfaces.
//
// THE WEBSOCKET DOES NOT USE THIS FUNCTION and must not: it has no HTTP status
// to return and no `service_unavailable` member to name. Its analogue of the
// 503 arm is close code 1013 Try Again Later, in
// internal/ws/handler.go:refuseHandshake — the same distinction, in the
// vocabulary that transport actually has (websocket-protocol.md §2.4, §6.2).
func AuthFailure(err error) (status int, code ErrorCode, message string) {
	if auth.IsLookupFailure(err) {
		return http.StatusServiceUnavailable,
			ErrCodeServiceUnavailable,
			"authentication is temporarily unavailable; retry shortly"
	}
	return http.StatusUnauthorized, ErrCodeAuthFailed, "invalid or expired token"
}
