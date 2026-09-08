package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TOMBSTONE SQL for the MYR-184 vehicle-sharing surface — every statement that
// ends a share, in one place.
//
// Split out of vehicle_share_queries.go by MYR-609, which gave the family a
// shared new obligation and pushed that file past the 300-line rule. The
// obligation: since migration 0051 a tombstone records not only THAT access
// ended but WHO ended it, in `revoked_by`. Five statements write one, and they
// are collected here so the vocabulary can be read as a whole rather than
// inferred from five scattered UPDATEs.
//
//	queryRevokeShare                 owner   §7.5.3, one row
//	queryRevokeSharesForVehicle      owner   vehicle offboarding, all rows
//	querySupersedeCollidingShares    owner   redeem-time supersede (MYR-609)
//	queryLeaveVehicleShares          grantee §7.5.7, the rider's own exit
//	queryRevokeSharesReceived        grantee account deletion (account_deletion_queries.go)
//
// ONLY 'grantee' BLOCKS AN EXTEND. §7.5.8 reads the newest tombstone for a
// (vehicle, grantee) pair and refuses when the grantee wrote it: leaving is the
// one exit a grantee has, and an owner who could re-grant the car on a button
// press would make that exit reversible by the party being left. An owner
// tombstone does not block — re-sharing a car you un-shared is the ordinary
// case — and NULL (any tombstone predating 0051) does not block either, which
// is the fail-open direction migration 0051 argues for.
//
// The two invariants at the top of vehicle_share_queries.go hold here too:
// every mutation carries its ownership predicate IN THE STATEMENT, and the read
// that decides is a locking SELECT inside the same transaction as the write.

// queryRevokeSharesForVehicle tombstones every live grant on a vehicle. Called
// from the owner-offboarding path (a car that has left the fleet must not keep
// appearing in its viewers' lists) and from the MYR-599 owner-wins transfer.
//
// IT RETURNS THE GRANTEES IT CUT, and that is not decoration (MYR-601). Every
// row this statement touches is somebody's live access to the car, and on the
// transfer path each of them is holding an open WebSocket the hub has to be
// told about — the cache TTL and the sweep are the alternative, and both are
// measured in minutes of a stranger's live GPS. `accepted_by_user_id` is NULL
// on a PENDING row (nobody redeemed it, so nobody is streaming), which the
// caller filters.
//
// The offboarding caller keeps using `tx.Exec`, which discards the rows: it is
// deleting the vehicle outright, so `Hub.RemoveVehicle` closes every subscribed
// session at once and there is no per-grantee question to answer.
const queryRevokeSharesForVehicle = `
UPDATE go_vehicle_shares
SET status = 'revoked', revoked_at = NOW(), revoked_by = 'owner'
WHERE vehicle_id = $1 AND status <> 'revoked'
RETURNING accepted_by_user_id`

// querySupersedeCollidingShares retires the locked PENDING rows that can never
// become grants: the redeemer already holds a LIVE ACCEPTED grant on that
// vehicle through some other invite, so `uq_go_vehicle_shares_accepted_grant`
// would refuse them (MYR-609). See supersedeCollidingRows for why the batch
// retires them instead of failing whole.
//
// $3 is the tombstone's author and $4 its reason. Both are BOUND rather than
// literal, unlike every other tombstone statement in this file: this is the one
// place the vocabulary constants in vehicle_share_types.go are the value
// written, and binding them is what keeps the Go names and the stored strings
// from drifting apart silently.
//
// `status = 'pending'` is re-asserted even though the ids came from an
// already-locked pending SELECT in the same transaction — invariant (1) at the
// top of this file, the same belt and braces queryAcceptSharesByID keeps.
//
// The EXISTS deliberately excludes the row being examined by keying on
// `status = 'accepted'`: a pending row can never satisfy it, so a code cannot
// supersede itself.
const querySupersedeCollidingShares = `
UPDATE go_vehicle_shares AS s
SET status = 'revoked', revoked_at = NOW(), revoked_by = $3, revoked_reason = $4
WHERE s.id = ANY($1) AND s.status = 'pending'
  AND EXISTS (
    SELECT 1 FROM go_vehicle_shares live
    WHERE live.vehicle_id = s.vehicle_id
      AND live.accepted_by_user_id = $2
      AND live.status = 'accepted')`

// queryRevokeShare is the tombstone flip. Guarded on owner_user_id (nobody
// revokes another owner's grant) and on `status <> 'revoked'` so a repeat is a
// no-op rather than a second revoked_at stamp. Zero rows affected means either
// "already revoked" or "not yours / does not exist"; the caller disambiguates
// with queryShareExistsForOwner, because those two must produce DIFFERENT
// statuses (204 idempotent vs 404) while remaining indistinguishable from the
// outside for a row the caller has no business seeing.
//
// RETURNING carries the accepted_by_user_id (empty for a pending row) so the
// caller can bust that viewer's cached access set immediately — otherwise a
// revoked viewer keeps resolving the vehicle for up to the cache TTL.
//
// It also carries vehicle_id, which is what lets the caller tear down the
// revoked viewer's LIVE WebSocket for that one car rather than every session
// they hold (MYR-373, websocket-protocol.md §10 DV-09). Read in the same
// statement that did the revoking, so a concurrent edit cannot make the id and
// the revocation disagree.
// `revoked_by = 'owner'` (migration 0051) records the AUTHOR, which §7.5.8
// extend reads before re-granting. An owner tombstone deliberately does NOT
// block a later extend: an owner re-sharing a car they themselves un-shared is
// the ordinary case. Written as a literal rather than a bind because this
// statement has exactly one caller and exactly one possible author.
const queryRevokeShare = `
UPDATE go_vehicle_shares
SET status = 'revoked', revoked_at = NOW(), revoked_by = 'owner'
WHERE id = $1 AND owner_user_id = $2 AND status <> 'revoked'
RETURNING COALESCE(accepted_by_user_id, ''), vehicle_id`

// MYR-469 — the RIDER's own way out of a share. Tombstones every ACCEPTED
// grant the caller redeemed on this vehicle — the same tombstone the owner's
// revoke writes (status → revoked, revoked_at stamped; never a hard delete,
// for the same audit reason) — and refuses ATOMICALLY while the caller has a
// live ride on the car: the ride's telemetry access rides the grant, so a
// leave mid-ride is the MYR-449 dark stream self-inflicted. The NOT EXISTS is
// inside the statement so a ride created between a check and the write cannot
// slip through the gap.
// `revoked_by = 'grantee'` (migration 0051) IS THE LOAD-BEARING HALF OF THIS
// STATEMENT SINCE MYR-609. It is the only record that the person LEFT rather
// than was removed, and §7.5.8 extend refuses on it: without the stamp, an
// owner could hand back the access the grantee walked away from, with no act
// by the grantee and no notification to them, turning the one exit a grantee
// has into something the party they were leaving can undo.
const queryLeaveVehicleShares = `
UPDATE go_vehicle_shares
SET status = 'revoked', revoked_at = NOW(), revoked_by = 'grantee'
WHERE vehicle_id = $1 AND accepted_by_user_id = $2 AND status = 'accepted'
  AND NOT EXISTS (
    SELECT 1 FROM go_ride_requests r
    WHERE r.vehicle_id = $1 AND r.rider_id = $2
      AND r.status IN ('requested', 'accepted', 'arrived', 'enroute'))`

// queryViewerLeaveRefused disambiguates a zero-row leave: it answers a row
// exactly when an accepted grant EXISTS and a live ride held it in place —
// i.e. the guard fired. BOTH conditions, deliberately: a caller with a live
// ride and no grant at all (an owner self-riding a never-shared car) has
// nothing to leave, and answering 409 there would be a refusal about a share
// they do not hold. No row → nothing to leave → idempotent success.
const queryViewerLeaveRefused = `
SELECT 1 FROM go_vehicle_shares s
WHERE s.vehicle_id = $1 AND s.accepted_by_user_id = $2 AND s.status = 'accepted'
  AND EXISTS (
    SELECT 1 FROM go_ride_requests r
    WHERE r.vehicle_id = $1 AND r.rider_id = $2
      AND r.status IN ('requested', 'accepted', 'arrived', 'enroute'))
LIMIT 1`

// revokeSharesReturningGrantees tombstones every live grant on the car and
// reports WHO it cut.
//
// THE IDS ARE THE POINT (MYR-601). The revocation itself is MYR-599 behaviour
// and unchanged; what was missing is that nothing told the running process
// about it. Each of these accounts may be holding an open WebSocket whose
// access set was frozen while the grant was live, and `Client.vehicleIDs`
// still names this car — so without the ids the only things that would end
// their stream are the 5-minute access-cache TTL and the 60-second sweep.
//
// A PENDING row carries a NULL grantee (nobody redeemed it), and duplicates
// are possible (one person can hold a grant and an unredeemed invite on the
// same car). Both are filtered here rather than at the call site, so the
// caller receives a list it can hand to the hub one id at a time.
func revokeSharesReturningGrantees(ctx context.Context, tx pgx.Tx, vehicleID string) ([]string, error) {
	rows, err := tx.Query(ctx, queryRevokeSharesForVehicle, vehicleID)
	if err != nil {
		return nil, fmt.Errorf("revokeSharesReturningGrantees(vehicle=%s): %w", vehicleID, err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	var out []string
	for rows.Next() {
		var grantee *string
		if err := rows.Scan(&grantee); err != nil {
			return nil, fmt.Errorf("revokeSharesReturningGrantees(vehicle=%s): scan grantee: %w", vehicleID, err)
		}
		if grantee == nil || *grantee == "" {
			continue
		}
		if _, dup := seen[*grantee]; dup {
			continue
		}
		seen[*grantee] = struct{}{}
		out = append(out, *grantee)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("revokeSharesReturningGrantees(vehicle=%s): %w", vehicleID, err)
	}
	return out, nil
}
