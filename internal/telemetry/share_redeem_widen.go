package telemetry

// THE LIVE-SOCKET HALF OF A REDEMPTION (MYR-601) — split from
// share_redeem_handler.go, which the seam pushed past the 300-line rule.
//
// The seam is the same one every other file in this package draws: the handler
// owns the request, the response and the refusals; this owns the announcement
// that the redemption changed somebody's access set. They are wired together
// and they fail apart — a redemption with no widener still redeems.

// ShareRedeemOption configures the redeem handler.
type ShareRedeemOption func(*ShareRedeemHandler)

// WithShareRedeemWidener wires the live-socket half of a redemption (MYR-601).
//
// THE CACHE BUST WAS NEVER ENOUGH ON ITS OWN, and redeem is the path where that
// shows most plainly. The bust fixes the NEXT handshake; a redeemer who was
// already connected — which is every redeemer who tapped an invite link inside
// the app — holds a session whose access set was frozen before the grant
// existed. Their "you're in!" screen is followed by a tracking sheet that
// cannot subscribe to the car, which is the same failure §7.5.5's own cache
// bust exists to prevent, one layer down.
//
// Nil in dev mode and in tests that wire no bus.
func WithShareRedeemWidener(wd ShareAccessWidener) ShareRedeemOption {
	return func(h *ShareRedeemHandler) { h.widened = wd }
}
