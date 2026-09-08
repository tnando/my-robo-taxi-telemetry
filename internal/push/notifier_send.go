package push

import (
	"context"
	"log/slog"
)

// Fan-out and delivery for the Notifier. Split from notifier.go to keep both
// files inside the 300-line cap.

// delivery is everything about ONE fan-out that is not the copy: who it is for,
// what it is about, and — since MYR-349 — which switch in their Settings turns
// it off. Grouped into a struct because four positional strings at a call site
// is exactly how a topic ends up in the ride-id slot.
type delivery struct {
	// userID is the RECIPIENT — the owner for a new request, the rider for a
	// status change or a due reservation. The preference consulted is theirs,
	// never the other party's.
	userID string
	rideID string
	topic  string
	// category is the preference this notification answers to. Every
	// preference-gated fan-out site must name one; there is no unset value
	// that means "always send" — the ONLY way past the gate is the explicit
	// `transactional` flag below, and fanOut refuses a delivery that claims
	// neither.
	category Category
	// transactional marks an OPERATIONAL ACCOUNT NOTICE rather than a
	// subscribable feed, and it is the one thing that bypasses the §7.19
	// preference gate (MYR-592, rest-api.md §7.19.4).
	//
	// WHY A FLAG AND NOT A SIXTH CATEGORY. A category is a switch, and the
	// premise of a switch is that turning it off is a choice the platform will
	// honour. The day-4 inactivity warning is not that kind of message: it is
	// the platform telling an owner it is about to stop a service they are
	// paying for, one day before it does, and the only alternative channel is
	// an in-app notice they will not see precisely because not opening the app
	// is what triggered it. Minting a `telemetrySuspension` category would have
	// put a toggle on the owner's Settings screen whose honest label is "do not
	// warn me before I lose live telemetry" — a switch nobody wants, that
	// silently defeats the feature, and that §7.19's own framing forbids
	// ("These are DELIVERY preferences, not authorization... A category that is
	// off is a silence").
	//
	// USE IT ONLY FOR NOTICES THAT MEET ALL THREE TESTS: the platform is acting
	// on the account rather than reporting somebody else's action, the recipient
	// can still prevent or reverse it, and no other channel reaches them in
	// time. Ride news fails the first test; a marketing message fails all three.
	// Today this is the only such delivery in the service, and it should stay
	// rare enough to enumerate.
	transactional bool
	// islandAlerts marks a delivery whose news the recipient's Live Activity
	// is ALSO about to deliver, by expanding the Dynamic Island (MYR-413).
	//
	// False is the safe default and the common one: an unset field means "send
	// the banner", so a new fan-out site that forgets this cannot accidentally
	// silence itself. See notifier_activity_gate.go.
	islandAlerts bool
	// tripPush is the MYR-602 subject: the trip, the car, the event and the
	// deep link that together replace `rideID` on a notification that is not
	// about a ride. Nil on every ride delivery, which is what keeps their
	// payloads byte-identical (see buildPayload).
	//
	// A POINTER rather than a value so that "this is not a trip push" is one
	// nil check rather than a comparison against a zero struct — the same
	// reason ActivityNotification.Alert is one.
	tripPush *TripPush
	// legActivity marks a delivery whose news a LEG's Live Activity is also
	// carrying, on every phone registered for the trip (MYR-620).
	//
	// The trips sibling of islandAlerts, and separate from it because the two
	// consult different registries about different subjects: that one asks
	// go_live_activities about a RIDE, this one asks the push-to-start registry
	// about a TRIP. False is the safe default and the common one — a lifecycle
	// trip push and every ride push leave it unset and keep their banner.
	legActivity bool
}

// fanOut resolves one ride party's devices and sends the alert to each. Every
// failure here is logged and swallowed: a notification is best-effort garnish
// on a ride that has already happened.
func (n *Notifier) fanOut(ctx context.Context, d delivery, a alert) {
	userID, rideID, topic := d.userID, d.rideID, d.topic
	if userID == "" {
		return
	}

	if !n.active() {
		// Keyless or kill-switched. Log the intent so an operator can see the
		// pipeline is alive and only the delivery is missing — this is the
		// normal state before APNS_KEY_P8 is set on the deploy.
		n.logger.Info("push skipped",
			slog.String("topic", topic),
			slog.String("ride_id", rideID),
			slog.String("user_id", userID),
			slog.Bool("push_enabled", n.cfg.Enabled),
			slog.Bool("apns_configured", n.sender != nil),
		)
		return
	}

	// MYR-592 — a delivery must declare itself: either it answers to a
	// preference switch or it is an operational account notice. NEITHER is a
	// programming error, and it is refused rather than sent, because the
	// pre-existing fall-through was a SILENT BYPASS — Prefs.Allows returns true
	// for a category it does not recognise (the safe direction for a column the
	// model has not grown yet), so an empty category sailed past the gate and
	// looked exactly like a transactional send nobody authorised.
	if d.category == "" && !d.transactional {
		n.logger.Error("push: delivery declares neither a category nor transactional intent; refusing",
			slog.String("topic", topic),
			slog.String("user_id", userID),
		)
		return
	}

	// MYR-349 — the recipient's own switch, checked BEFORE the device lookup so
	// a silenced category costs one point read instead of a fan-out.
	//
	// MYR-592 — skipped entirely for a transactional notice. See delivery.
	if !d.transactional && !n.allowed(ctx, userID, d.category, topic) {
		return
	}

	// MYR-413 — is this news already on its way to a Dynamic Island the
	// recipient is watching? Checked per RECIPIENT rather than per ride,
	// because the owner and the rider both get pushes on one transition and
	// only the rider has a card. Second rather than first, so a rider who
	// muted the category costs one read instead of two.
	if n.duplicatesLiveActivity(ctx, d) {
		return
	}

	// MYR-620 — the trips sibling of the same question: is this leg's card
	// already on this recipient's lock screen? A phone registered for the
	// trip's push-to-start gets the card and NOT the banner; a phone with Live
	// Activities disabled never registers, so it is told in prose. Checked per
	// RECIPIENT for the same reason: the audience is mixed by construction.
	if n.holdsLegActivity(ctx, d) {
		return
	}

	devices, err := n.stores.devices.DevicesForUser(ctx, userID)
	if err != nil {
		n.logger.Error("push: device lookup failed",
			slog.String("topic", topic),
			slog.String("ride_id", rideID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return
	}
	if len(devices) == 0 {
		n.logger.Debug("push: no devices registered",
			slog.String("topic", topic),
			slog.String("user_id", userID),
		)
		return
	}

	var delivered int
	for _, dev := range devices {
		if n.send(ctx, dev, d, a) {
			delivered++
		}
	}

	// P1 discipline: the audit line carries opaque ids and counts only — never
	// a device token, and never the alert copy, which embeds a first name.
	n.logger.Info("push sent",
		slog.String("topic", topic),
		slog.String("ride_id", rideID),
		slog.String("trip_id", d.tripID()),
		slog.String("user_id", userID),
		slog.Int("devices", len(devices)),
		slog.Int("delivered", delivered),
	)
}

// allowed reports whether the recipient wants this category of notification
// (MYR-349). It is the ONE gate; every send site reaches Apple through it.
//
// IT FAILS OPEN, in all three of the ways it can fail: no PrefStore wired, a
// lookup error, and an account with no stored row. All three resolve to
// DefaultPrefs — everything on, i.e. the exact pre-MYR-349 behaviour.
//
// That direction is not a shortcut, it is the product decision. This package's
// standing rule is that a duplicate notification is a minor annoyance to a
// human whereas a missed one is a rider standing on a sidewalk, and a
// preference gate that failed CLOSED would convert every transient database
// hiccup into platform-wide silence — with no error surfacing anywhere, because
// nothing about a ride waits on push. The cost of failing open is bounded and
// visible: somebody who switched a category off might occasionally receive it.
func (n *Notifier) allowed(ctx context.Context, userID string, category Category, topic string) bool {
	if n.stores.prefs == nil {
		return true
	}

	prefs, err := n.stores.prefs.PrefsForUser(ctx, userID)
	if err != nil {
		n.logger.Error("push: prefs lookup failed; sending anyway",
			slog.String("topic", topic),
			slog.String("category", string(category)),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return true
	}
	if prefs.Allows(category) {
		return true
	}

	// P0 throughout: an opaque cuid and a category name. Logged at Info rather
	// than Debug because "the notification you expected never arrived" is a
	// support question, and this line is the whole answer to it.
	n.logger.Info("push suppressed by preference",
		slog.String("topic", topic),
		slog.String("category", string(category)),
		slog.String("user_id", userID),
	)
	return false
}
