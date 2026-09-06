package ws

import (
	"context"
	"log/slog"
	"maps"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// THE RE-MASK — the half of the sweep that MYR-602 needed, split from
// access_revalidator.go so both stay inside the 300-line cap.
//
// The seam is not arbitrary. Its neighbour answers "may this session still see
// these cars at all?", and its answer is binary and its remedy is a KICK. This
// file answers "at what TIER?", and its answer is a table and its remedy is a
// re-projection of a connection that stays open. The two run in one pass
// because they read the same client list; they are otherwise unrelated
// mechanisms with different failure modes, and the kick deliberately wins when
// both apply.

// remaskClient re-resolves this session's per-vehicle roles and republishes the
// table when any of them moved. Reports whether it republished.
//
// THE WHOLE TABLE IS REPLACED, never edited: Client.roles is read lock-free on
// the broadcast hot path, and the immutability of a published table is what
// makes that safe. See Client.setRoles.
//
// IT RESOLVES ONLY THE VEHICLES THE CLIENT HOLDS, memoized per (user, vehicle)
// across the pass — the same shape as the access read above. The cost is one
// ResolveRole per (distinct user, vehicle) per minute, which at this fleet's
// size is a handful of indexed reads; if it ever stops being, the fix is a
// batch resolver, not a wider interval, because the interval IS the
// window-edge latency. See memoizedRole for the failure posture.
//
// AND A REPUBLISH IS FOLLOWED BY A SYNTHESIZED SNAPSHOT for every vehicle whose
// role moved. A new mask governs future frames and says nothing about the state
// the client is already holding; without the re-delivery a narrowing leaves a
// real frozen coordinate on the map and a promotion leaves the sentinel zeros.
// See resendSnapshots.
func (r *AccessRevalidator) remaskClient(
	ctx context.Context, client *Client, memo map[userVehicle]auth.Role,
) bool {
	// The vehicles whose role MOVED, collected inside the lock and acted on
	// outside it: each one needs a synthesized snapshot afterwards (see
	// resendSnapshots), and a database read plus a marshal do not belong under
	// a lock the next re-mask of this session waits on.
	var moved []string

	// THE WHOLE READ-MODIFY-WRITE IS UNDER THE CLIENT'S OWN LOCK, resolution
	// included. SweepOnce is reachable from four places (its ticker and the
	// trips service's nudge on three kinds of edge), and without this a
	// narrowing publish could be overwritten by a concurrent pass that had
	// already read the pre-narrowing table — leaving a participant whose window
	// closed still holding live location. Holding the lock across the resolve
	// means the losing pass re-reads after the winner's write instead of
	// replaying a decision made before it.
	republished := client.withRoles(func(current roleTable) map[string]auth.Role {
		if len(current) == 0 {
			// Nothing published: either a dev-mode wildcard client (whose role
			// comes from defaultRole and is not per-vehicle) or a handshake
			// whose every ResolveRole failed. Neither has a table to narrow,
			// and minting one here would be this sweep deciding a role the
			// handshake refused to.
			return nil
		}

		next := maps.Clone(current)
		changed := false
		for vehicleID, held := range current {
			role, ok := r.memoizedRole(ctx, memo, client.userID, vehicleID, held)
			if !ok || role == held {
				continue
			}
			next[vehicleID] = role
			moved = append(moved, vehicleID)
			changed = true
			r.logger.Info("access revalidation: role changed mid-connection",
				slog.String("user_id", client.userID),
				slog.String("vehicle_id", vehicleID),
				slog.String("from", held.String()),
				slog.String("to", role.String()),
			)
		}
		if !changed {
			return nil
		}
		return next
	})
	if !republished {
		return false
	}

	r.resendSnapshots(ctx, client, moved)
	return true
}

// userVehicle keys the per-pass role memo.
type userVehicle struct{ userID, vehicleID string }

// memoizedRole resolves one (user, vehicle) role at most once per pass.
//
// A FAILED RESOLUTION KEEPS THE OLD ROLE and is NOT memoized: the fallback is
// this session's currently held role, which is a different value for a
// different session, so caching it would spread one session's answer to
// another's. Failing open is right in both directions here and neither is a
// judgement call: keeping the old role means a narrowing is delayed by one
// interval (bounded, and the same bound the whole sweep already has), while
// falling back to the deny-all sentinel would black out a live map on a
// database blip, and falling back to the strongest role would hand out location
// on one. Keeping what was already true is the only answer that invents
// nothing.
func (r *AccessRevalidator) memoizedRole(
	ctx context.Context, memo map[userVehicle]auth.Role, userID, vehicleID string, held auth.Role,
) (auth.Role, bool) {
	key := userVehicle{userID: userID, vehicleID: vehicleID}
	if role, ok := memo[key]; ok {
		return role, true
	}
	role, err := r.resolveRole(ctx, userID, vehicleID)
	if err != nil {
		r.logger.Warn("access revalidation: role resolve failed; keeping the current role",
			slog.String("user_id", userID),
			slog.String("vehicle_id", vehicleID),
			slog.String("role", held.String()),
			slog.Any("error", err),
		)
		return auth.Role(""), false
	}
	memo[key] = role
	return role, true
}

// resendSnapshots re-delivers the whole known state of each re-masked vehicle
// through the client's NEW mask.
//
// WITHOUT IT A RE-MASK IS HALF A CHANGE, and it is half in both directions.
// A WebSocket client holds a MERGED state assembled from every frame it has
// received; the mask decides what future frames carry, and it can say nothing
// at all about what the client is already holding.
//
//   - NARROWED (trip_participant → viewer). The client is holding the last
//     REAL coordinate the car sent. From here on, a location-only delta
//     projects for a viewer to nothing but sentinels and is suppressed by
//     IsSubstantiveExcludingSentinels — correctly, because its arrival would
//     be a "this car is streaming" beacon — so no later frame ever overwrites
//     that coordinate. The map would keep showing a real, frozen position for
//     the whole life of the connection. The narrowing would have removed the
//     stream and left the snapshot.
//   - PROMOTED (viewer → trip_participant). The client is holding the SENTINEL
//     zeros its connect-time snapshot delivered, and nothing corrects them
//     until the car happens to send a location group. A parked car sends none:
//     the participant's map would show Null Island for exactly as long as the
//     car stayed still, which on a road trip is every overnight stop.
//
// A snapshot re-delivery answers both with one mechanism, and with the same
// code path the connect-time delivery uses — so the projection, the atomic
// grouping and the sentinel substitution cannot drift between "you subscribed"
// and "your role changed". It is bounded by the number of vehicles whose role
// actually MOVED, which is zero on almost every pass.
func (r *AccessRevalidator) resendSnapshots(ctx context.Context, client *Client, vehicleIDs []string) {
	if r.hub == nil {
		return
	}
	for _, vehicleID := range vehicleIDs {
		r.hub.sendSnapshot(ctx, client, vehicleID, resolveTimeout)
	}
}
