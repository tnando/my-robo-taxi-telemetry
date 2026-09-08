package ws

import (
	"context"
	"fmt"
	"time"
)

// WHAT A USER MAY SEE, AND HOW THE SWEEP READS IT — split from
// access_revalidator.go so that file stays inside the 300-line rule once
// MYR-601 gave the sweep its third arm.
//
// Everything here is about the ANSWER (one user's current entitlement) rather
// than about what the sweep does with it: the two closing arms live in
// access_revalidator.go and access_widen_backstop.go, and the tier arm in
// access_remask.go.

// accessSet is one user's current entitlement.
//
// The `all` flag is why this is a struct and not a bare map. "All access" and
// "no access" both produce zero per-vehicle entries, and they are opposite
// answers: the first must close nothing, the second must close everything that
// user has open. A bare map would have to encode the difference as nil-versus-
// empty, which is exactly the distinction a future edit drops.
type accessSet struct {
	// all is the dev-mode wildcard sentinel: authorized for every vehicle.
	all bool
	// allowed holds the concrete vehicle ids. Empty and non-nil is a real
	// answer — the user may see nothing.
	allowed map[string]struct{}
}

// resolveTimeout caps a single resolver call. The sweep walks every connected
// user on one goroutine, so a hung database read would wedge the whole pass
// and, with it, every later pass — the backstop would go quiet exactly when
// the thing it backstops is most likely to be struggling. Nothing on the path
// today blocks unboundedly; this is the guard that keeps that true.
const resolveTimeout = 5 * time.Second

// resolve fetches the user's current entitlement.
func (r *AccessRevalidator) resolve(ctx context.Context, userID string) (accessSet, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	ids, err := r.resolver.GetUserVehicles(ctx, userID)
	if err != nil {
		return accessSet{}, fmt.Errorf("revalidator.resolve(user=%s): %w", userID, err)
	}
	out := accessSet{allowed: make(map[string]struct{}, len(ids))}
	for _, id := range ids {
		if id == WildcardVehicleID {
			return accessSet{all: true}, nil
		}
		out.allowed[id] = struct{}{}
	}
	return out, nil
}

// firstLostVehicle reports a vehicle the client is still holding that the
// user's current entitlement no longer covers. One is enough: RevokeUserAccess
// closes the whole session, so finding a second would change nothing.
func firstLostVehicle(c *Client, set accessSet) (string, bool) {
	if set.all {
		return "", false
	}
	for _, held := range c.authorizedVehicles() {
		if _, ok := set.allowed[held]; !ok {
			return held, true
		}
	}
	return "", false
}
