package push

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// The TRIP-LEG Live Activity sender (MYR-602).
//
// It is a SEPARATE type from ActivityNotifier rather than a mode of it, and the
// split is by LIFECYCLE, not by taste. That notifier is a bus subscriber: it
// exists to turn `ride.status.changed` into a push, it owns a subscription and
// a drain, and its whole vocabulary — terminalStatuses, the six-rung alert
// ladder, the held completion `end`, the progress anchor — is ride vocabulary
// that a leg has no counterpart for. This type is a plain callee: the leg
// detector, which already knows exactly what happened and to whom, calls it.
//
// WHAT IS SHARED, deliberately and completely: the APNs client, the payload
// builder, the priority rule, the expiration rule, the stale-date, and the
// alert-on-update-then-end pattern. Those are the parts MYR-418 proved this
// surface cannot afford two copies of — an alert on an `end` is accepted by
// APNs and honoured by nothing, so a second wrong implementation would look
// exactly like a working one from the server all the way to the logs.
//
// THREE MOMENTS IN A LEG'S CARD:
//
//	StartLeg   push-to-start, addressed by go_trip_activity_tokens. The server
//	           CREATES the card, because a leg begins while no participant's
//	           phone is doing anything.
//	UpdateLeg  ordinary content-state replacement, addressed by the per-Activity
//	           update tokens the started cards register through §7.21.
//	EndLeg     an ALERTING UPDATE and then an `end`, in that order and with
//	           distinct timestamps. See EndLeg.

// TripActivityNotifier pushes trip-leg Live Activities.
type TripActivityNotifier struct {
	sender ActivitySender
	store  TripActivityStore
	prefs  PrefStore
	cfg    Config
	logger *slog.Logger
	// now is the injectable clock. Timestamps and stale-dates are the whole
	// contract of this surface, so tests pin them rather than tolerate them.
	now func() time.Time
}

// NewTripActivityNotifier builds the leg-card sender.
//
// sender may be nil — the keyless mode the service runs in before the APNs
// secrets are set, where every send is logged as skipped. prefs may be nil,
// meaning every category is on; that is the same fail-open direction every
// other gate in this package takes, and for the same reason.
func NewTripActivityNotifier(
	sender ActivitySender,
	store TripActivityStore,
	prefs PrefStore,
	cfg Config,
	logger *slog.Logger,
) *TripActivityNotifier {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &TripActivityNotifier{
		sender: sender,
		store:  store,
		prefs:  prefs,
		cfg:    cfg.withDefaults(),
		logger: logger,
		now:    time.Now,
	}
}

// active reports whether a send would actually reach Apple.
func (t *TripActivityNotifier) active() bool {
	return t.cfg.Enabled && t.sender != nil && t.store != nil
}

// allowed applies the `trips` preference gate.
//
// THE CATEGORY IS THE DIFFERENCE FROM THE RIDE PATH, whose twin of this method
// hardcodes CategoryRideLifecycle. A person who switched trips off must get no
// trip card, and — the direction that actually matters — a person who switched
// RIDES off must still get their trip cards, because the two are unrelated
// products sharing one transport.
//
// Fails OPEN in all three of its failure modes (no store, a lookup error, an
// account with no row), exactly as its ride twin does: this package's standing
// rule is that a duplicate notification annoys a human while a missed one
// leaves somebody with nothing, and a gate that failed closed would turn a
// database hiccup into platform-wide silence.
func (t *TripActivityNotifier) allowed(ctx context.Context, userID, legID string) bool {
	if t.prefs == nil || userID == "" {
		return true
	}
	prefs, err := t.prefs.PrefsForUser(ctx, userID)
	if err != nil {
		t.logger.Error("trip activity: prefs lookup failed; sending anyway",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return true
	}
	if prefs.Allows(CategoryTrips) {
		return true
	}
	t.logger.Info("trip activity suppressed by preference",
		slog.String("category", string(CategoryTrips)),
		slog.String("user_id", userID),
		slog.String("leg_id", legID),
	)
	return false
}

// StartLeg push-to-starts one leg's card on every registered phone.
//
// Reports how many cards it asked Apple to create. Zero is an ordinary result —
// a trip whose participants are all on the web, or one nobody has opened on an
// iPhone — and is never an error: the leg's pushes still go out through the
// ordinary notifier, which is what the "a leg that never got a token
// registration still gets its pushes" rule means.
func (t *TripActivityNotifier) StartLeg(ctx context.Context, tc TripLegContext) int {
	if !t.active() {
		t.logger.Debug("trip activity start skipped",
			slog.String("leg_id", tc.LegID),
			slog.Bool("push_enabled", t.cfg.Enabled),
			slog.Bool("apns_configured", t.sender != nil),
		)
		return 0
	}

	tokens, err := t.store.PushToStartTokensForTrip(ctx, tc.TripID)
	if err != nil {
		t.logger.Error("trip activity: push-to-start registry lookup failed",
			slog.String("trip_id", tc.TripID),
			slog.String("error", err.Error()),
		)
		return 0
	}
	if len(tokens) == 0 {
		return 0
	}

	now := t.now()
	state := tripContentState(tc, now)
	// The LEG is REQUIRED in the attributes: without it the created card has no
	// anchor to register its own update token against and can never be updated
	// or ended — and the iOS struct declares it non-optional, so a payload
	// missing it fails the decode and raises no card at all.
	start := &TripActivityStart{
		TripID:      tc.TripID,
		LegID:       tc.LegID,
		VehicleID:   tc.VehicleID,
		VehicleName: tc.VehicleName,
	}

	var started int
	for _, tok := range tokens {
		if !t.allowed(ctx, tok.UserID, tc.LegID) {
			continue
		}
		err := t.sender.SendActivity(ctx, ActivityNotification{
			ActivityToken: tok.Token,
			Sandbox:       tok.Sandbox,
			Event:         ActivityEventStart,
			ContentState:  state,
			Timestamp:     now,
			Start:         start,
			// No Alert. The card APPEARING is the announcement, and the
			// `trip_leg_started` banner is already on its way; a third
			// interruption for one fact is what MYR-413 exists to stop.
		})
		switch {
		case err == nil:
			started++
		case errors.Is(err, ErrUnregistered):
			// THE APP is gone, not a card — this token addresses an
			// installation. The row goes from go_trip_activity_tokens, which is
			// a DIFFERENT table from the one dropActivity touches; see the
			// store file's header for why pointing the ride path at this
			// verdict would delete nothing and retry forever.
			t.dropPushToStartToken(ctx, tok.Token)
		default:
			t.logger.Warn("trip activity: push-to-start failed",
				slog.String("leg_id", tc.LegID),
				slog.String("push_to_start_token_prefix", tokenPrefix(tok.Token)),
				slog.String("error", err.Error()),
			)
		}
	}

	t.logger.Info("trip activity started",
		slog.String("trip_id", tc.TripID),
		slog.String("leg_id", tc.LegID),
		slog.Int("tokens", len(tokens)),
		slog.Int("started", started),
		slog.Bool("has_eta", state.ETA != nil),
	)
	return started
}

// UpdateLeg replaces the content-state on every card already running for a leg.
//
// It addresses the per-Activity UPDATE tokens the started cards registered
// through §7.21, not the push-to-start tokens — a card that has not registered
// yet simply is not in this list, and the next pass picks it up.
func (t *TripActivityNotifier) UpdateLeg(ctx context.Context, tc TripLegContext) int {
	return t.fanOutLeg(ctx, tc, legFanOut{event: ActivityEventUpdate, at: t.now()})
}

// EndLeg delivers the leg's final state and takes the card off the lock screen.
//
// TWO PUSHES, IN THIS ORDER, AND THIS IS THE MYR-418 RULE VERBATIM. An `end`
// that carries an `aps.alert` is accepted by APNs and expands nothing — Apple's
// documentation introduces the alert dictionary under `start` and `update` and
// says of `end` only to include the final content state, and the client's
// real-device ride proved it: the island never opened and the rider saw no
// arrival at all. So the announcement rides an alerting UPDATE sent immediately
// BEFORE the end.
//
// THE TWO TIMESTAMPS MUST DIFFER, which is why `at` is passed in rather than
// read from the clock twice. `aps.timestamp` is rendered in whole SECONDS and
// ActivityKit DISCARDS an update whose timestamp is not newer than the one it
// is showing — two calls to now() inside one second would stamp the identical
// integer and the ordering of the pair would rest on undefined behaviour. The
// end is stamped one second after the alerting update, deliberately.
//
// The rows are tombstoned only after the end has been attempted, so a failed
// end leaves a live row the next pass can retry against.
func (t *TripActivityNotifier) EndLeg(ctx context.Context, tc TripLegContext) {
	if !t.active() {
		return
	}
	at := t.now()

	// Rung one: the alerting update. This is the push that opens the island and
	// says the leg is over.
	t.fanOutLeg(ctx, tc, legFanOut{
		event: ActivityEventUpdate,
		at:    at,
		alert: legEndAlert(tc),
	})

	// Rung two: the end, one second later so its `aps.timestamp` is strictly
	// newer. DismissAfter rather than DismissPromptly: an arrival is the state
	// the participant most wants a moment with, and it is the same five minutes
	// a completed ride's card lingers for.
	dismiss := at.Add(DismissAfter)
	t.fanOutLeg(ctx, tc, legFanOut{
		event:     ActivityEventEnd,
		at:        at.Add(time.Second),
		dismissAt: &dismiss,
	})

	if _, err := t.store.EndActivitiesForLeg(ctx, tc.LegID); err != nil {
		t.logger.Error("trip activity: tombstone leg activities failed",
			slog.String("leg_id", tc.LegID),
			slog.String("error", err.Error()),
		)
	}
}

// dropPushToStartToken removes a push-to-start token APNs permanently rejected,
// on a context detached from the caller's (which the send may have consumed).
func (t *TripActivityNotifier) dropPushToStartToken(ctx context.Context, token string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()

	if err := t.store.DeleteRejectedPushToStartToken(ctx, token); err != nil {
		t.logger.Error("trip activity: delete rejected push-to-start token failed",
			slog.String("push_to_start_token_prefix", tokenPrefix(token)),
			slog.String("error", err.Error()),
		)
		return
	}
	t.logger.Info("trip activity: deleted unregistered push-to-start token",
		slog.String("push_to_start_token_prefix", tokenPrefix(token)),
	)
}
