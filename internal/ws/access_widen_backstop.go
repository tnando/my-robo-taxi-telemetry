package ws

import (
	"log/slog"
)

// THE WIDEN ARM OF THE SWEEP (MYR-601) — split from access_revalidator.go so
// both stay inside the 300-line rule, on the same seam access_remask.go uses.
//
// WHY IT HAD TO EXIST. Until MYR-601 the sweep only ever NARROWED: it closed a
// session holding a vehicle the user could no longer see, and re-masked the
// tiers of the ones it kept. That made every claim of the form "and the
// 60-second sweep catches it" true for a lost grant and FALSE for a gained
// one — the widening direction had event-driven producers and nothing behind
// them. A dropped publish, a mutation served by another machine, or a writer
// that never learned the rule (the Next.js app writes `"Vehicle"` rows
// directly, reaching neither this process's cache nor its bus — DV-09 (d))
// left the car missing from an open session for the whole life of that
// connection, and the documented ≤60s bound was describing a mechanism that
// was not there.
//
// It is the CHEAPEST honest backstop: the sweep already resolves each
// connected user's current access set from the database, so the gained
// vehicles are the same answer read in the other direction. No new query, no
// new topic, no new frame — the same `Hub.WidenUserAccess` and the same `4002`
// re-handshake every event-driven producer uses.
//
// A KICK STILL BEATS A WIDEN, and a widen still beats a re-mask. The three
// arms are ordered by what they cost the session: a lost vehicle must close it
// (the security direction), a gained one closes it too but only to hand the
// user MORE, and a tier change keeps it open. A client that both lost and
// gained is closed once, by the loss, and the reconnect resolves both.
//
// ONCE PER USER PER PASS. `WidenUserAccess` closes every session that user
// holds — it cannot find them by a vehicle they are not yet authorized for,
// which is the whole reason it exists — so the second tab of the same user
// would otherwise publish a second close for sessions that are already gone.

// widenUser re-handshakes every session belonging to this client's user, once
// per pass, and reports how many sessions it closed.
//
// The `announced` set is the per-pass memo: the same shape and the same reason
// as SweepOnce's `resolved` access memo one level up.
func (r *AccessRevalidator) widenUser(client *Client, gained string, announced map[string]struct{}) int {
	if _, done := announced[client.userID]; done {
		return 0
	}
	announced[client.userID] = struct{}{}

	// INFO, not Debug: reaching this line means an access set grew and NOTHING
	// announced it — either a producer's publish was dropped, or the write came
	// from a path that does not publish at all (the web app's direct `"Vehicle"`
	// insert is the known one). That is exactly the signal an operator wants,
	// and it is rare enough not to bury the quiet passes.
	r.logger.Info("access revalidation: access set GREW with nothing announcing it",
		slog.String("user_id", client.userID),
		slog.String("vehicle_id", gained),
	)
	return r.hub.WidenUserAccess(client.userID, gained, "revalidation_backstop")
}

// firstGainedVehicle reports a vehicle the user's current entitlement covers
// that this session was NOT authorized for. One is enough: the widening closes
// the whole session and the reconnect re-derives the entire set, so finding a
// second would change nothing.
//
// IT IS JUDGED AGAINST THE HANDSHAKE-FROZEN SET, not against
// authorizedVehicles(): `Client.vehicleIDs` is what `subscribe` is gated on and
// what the broadcast fan-out reads, so it is the set that decides whether this
// session can see the car at all. The subscription map is a subset of it by
// construction and would only blur the question.
//
// The wildcard is not a gain. A dev-mode client is authorized for everything
// already, and SweepOnce skips those before it gets here — this is the second
// guard, because the sentinel's two readings ("all" and "none") are the one
// distinction a future edit drops.
func firstGainedVehicle(c *Client, set accessSet) (string, bool) {
	if set.all || c.allVehicles {
		return "", false
	}
	held := make(map[string]struct{}, len(c.vehicleIDs))
	for _, vid := range c.vehicleIDs {
		held[vid] = struct{}{}
	}
	for vid := range set.allowed {
		if _, ok := held[vid]; !ok {
			return vid, true
		}
	}
	return "", false
}
