package ws

import (
	"github.com/myrobotaxi/telemetry/internal/auth"
)

// A CLIENT'S PER-VEHICLE ROLE TABLE: how it is published, how it is read on the
// broadcast hot path, and the one supported way to change it.
//
// Split from client.go so both stay inside the 300-line cap, and along the seam
// MYR-602 created: everything else about a Client is fixed at handshake and
// read afterwards, while this table is the one piece of a live session that
// MOVES — a trip window opens and closes on the CLOCK, with no mutation
// anywhere to hang a handshake off. Its concurrency story is therefore
// different from every other field's, and it is worth reading on its own page.

// roleTable is one immutable published snapshot of a client's per-vehicle
// roles. Named rather than left as a bare map so the atomic pointer's type
// reads as "a table", and so the immutability rule has somewhere to be
// written down: NOTHING may write into a roleTable after setRoles has
// published it. Changing a role means building a new map and swapping.
type roleTable map[string]auth.Role

// setRoles publishes a new role table, replacing whatever was there.
//
// The caller must not retain or mutate m afterwards — see roleTable. A nil m
// publishes an empty table rather than a nil pointer, so roleFor never has to
// nil-check the load.
func (c *Client) setRoles(m map[string]auth.Role) {
	table := make(roleTable, len(m))
	for vid, role := range m {
		table[vid] = role
	}
	c.roles.Store(&table)
}

// withRoles runs fn under the per-client role lock, handing it the currently
// published table and publishing whatever fn returns.
//
// The ONE supported way to change a published table. fn MUST NOT mutate the
// table it is given (see roleTable) and MUST NOT block indefinitely — it holds
// a lock the next re-mask of this session waits on. Returning nil publishes
// nothing, which is how "no change" is expressed without a second return value.
func (c *Client) withRoles(fn func(roleTable) map[string]auth.Role) bool {
	c.rolesMu.Lock()
	defer c.rolesMu.Unlock()

	next := fn(c.rolesSnapshot())
	if next == nil {
		return false
	}
	c.setRoles(next)
	return true
}

// rolesSnapshot returns the currently published table. Never nil.
func (c *Client) rolesSnapshot() roleTable {
	if table := c.roles.Load(); table != nil {
		return *table
	}
	return roleTable{}
}

// roleFor returns the role this client holds against vehicleID. Resolution
// order: (1) the per-vehicle roles table (seeded at handshake, replaced by the
// revalidator on a window edge); (2)
// for clients with allVehicles=true that lack a per-vehicle entry, the
// defaultRole set at handshake (the dev-mode NoopAuthenticator path);
// (3) the empty Role("") fail-closed sentinel, which the mask layer in
// internal/mask interprets as deny-all. Production clients (allVehicles=
// false) skip step 2, so a missing vehicleRoles entry stays deny-all.
func (c *Client) roleFor(vehicleID string) auth.Role {
	if c == nil {
		return auth.Role("")
	}
	if role, ok := c.rolesSnapshot()[vehicleID]; ok {
		return role
	}
	if c.allVehicles {
		return c.defaultRole
	}
	return auth.Role("")
}
