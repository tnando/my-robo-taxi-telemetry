package trips

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/push"
)

// WHEN A LEG BANNER MAY BE SENT AT ALL (MYR-620).
//
// THE CLIENT'S SCREENSHOT, 2026-09-08: ten "Tesla is on the move — Heading to
// Element by Marriott Sedona." banners on one lock screen in 59 minutes, five
// inside a single minute, plus an older one for a Subway. Thomas's reading was
// *"users reported app spamming notifications when heading to next destination,
// this should be moving into dynamic island"*, and both halves of that sentence
// are rules now.
//
// EVERY ONE OF THOSE PUSHES WAS CORRECT BY THE OLD RULES. `trip_leg_started` is
// claimed once per leg on `go_trip_legs.started_notified_at`, and it was: the
// LEG flapped (MYR-612 — a transient destination-name delta closed the leg and
// the next frame reopened it), so each banner belonged to a different row. A
// per-row claim can never bound a sentence that is about the JOURNEY.
//
// So two gates sit in front of a leg banner, and they answer different
// questions:
//
//	IS THERE A BETTER SURFACE?  A phone holding a push-to-start registration for
//	                            this trip is getting a Live Activity for the leg
//	                            — the card IS the announcement, and a banner on
//	                            top of it is the thing standing in front of the
//	                            island the client asked us to use. That gate is
//	                            per RECIPIENT and lives in internal/push, beside
//	                            the MYR-413 ride gate it is the trips sibling of.
//	HAS IT ALREADY BEEN SAID?   This file. Per (trip, event, destination), once
//	                            per LegBannerWindow, whatever the detector does.
//
// The second is deliberately NOT conditional on the first: it protects the
// token-less phone, which is the only one that ever sees a leg banner now.

// legBannerAllowed reports whether this leg's banner may go out, and records
// the send when it may.
//
// IT FAILS OPEN. A store error sends the banner, for this package's standing
// reason: a duplicate notification is an annoyance and a missing one is a
// person who was never told their car set off. The flap that motivated the gate
// is rare; a database hiccup is not evidence one is happening.
func (s *Service) legBannerAllowed(ctx context.Context, leg Leg, event push.TripEvent) bool {
	allowed, err := s.legs.ClaimLegBannerSlot(
		ctx, leg.TripID, string(event), destinationKey(leg.DestinationName),
		s.now(), s.cfg.LegBannerWindow,
	)
	if err != nil {
		s.logger.Warn("trips: leg banner slot claim failed; sending the banner anyway",
			slog.String("leg_id", leg.ID),
			slog.String("event", string(event)),
			slog.String("error", err.Error()))
		return true
	}
	if !allowed {
		// P0 only — a leg id and an event. Logged at Info rather than Debug
		// because "the notification I expected never arrived" is a support
		// question and this line is the whole answer to it.
		s.logger.Info("trips: leg banner suppressed; the same sentence went out recently",
			slog.String("trip_id", leg.TripID),
			slog.String("leg_id", leg.ID),
			slog.String("event", string(event)),
			slog.Duration("window", s.cfg.LegBannerWindow))
	}
	return allowed
}

// destinationKey turns a destination name into the opaque key the suppression
// slot is stored under.
//
// A DIGEST, BECAUSE THE NAME IS P1. A destination is a place a car actually
// drove to (data-classification.md §1.18) and every other column holding one in
// this schema is sealed; `go_trip_leg_banners` holds none. Equality is the only
// operation the predicate needs, so hashing costs the feature nothing.
//
// NORMALISED FIRST, because the dash is not a database. Tesla re-sends a
// destination name on every re-route and the casing and the inner whitespace
// are not stable across those; two spellings of one hotel must land in one
// slot, or the suppression is defeated by a space.
//
// An empty name yields an empty key, which ClaimLegBannerSlot reads as "always
// allowed": a banner with no place in it says nothing that could be repeated,
// and one shared slot for every nameless leg would collapse genuinely different
// journeys.
func destinationKey(name string) string {
	normalised := strings.ToLower(strings.Join(strings.Fields(name), " "))
	if normalised == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalised))
	return hex.EncodeToString(sum[:])
}
