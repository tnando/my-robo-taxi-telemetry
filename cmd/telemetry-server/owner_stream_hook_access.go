package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-601 — THE ACCESS-SET HALF OF PROVISIONING A CAR.
//
// Observed live on 2026-09-06 during a second-car link: the row existed at
// 05:24:21Z, GET /api/vehicles rendered the new car immediately, and for the
// next four minutes every WebSocket handshake reported the OLD vehicle count
// and every `subscribe` for the new car was refused `vehicle_not_owned`. At
// 05:28:42Z a handshake finally saw four cars and the subscribe succeeded.
//
// Nothing was broken in the WS layer. The link path simply never told anybody
// the access set had grown, so both of the mechanisms that exist for exactly
// this had to time out instead:
//
//   - the REST access-set cache (auth.vehicleCache, 5-minute TTL) was warm from
//     before the link — the app was connected — so every handshake in that
//     window resolved the PRE-LINK set from memory. That is the ~3 minutes;
//   - and `Client.vehicleIDs` is frozen at handshake, so even a handshake that
//     HAD seen the car would not have helped the session already open.
//
// This is MYR-609's finding 4, arriving from the other end: MYR-609 fixed the
// share-extend producer and recorded that any other widening producer should
// use the same signal rather than invent a second one. So this file is a
// PRODUCER, not a mechanism — everything it does is one cache bust followed by
// one publish on `share.access_widened`, which is the topic MYR-609 already
// wired to Hub.WidenUserAccess.
//
// THE TOPIC'S NAME IS HISTORICAL. It says `share` because a share extend was
// the first widening that shipped; it means "this user's vehicle access set
// grew", and a provisioning is exactly that. Renaming it would be a wire-free
// but noisy change to a topic three files subscribe to, for no gain.

// accessSetInvalidator busts one user's cached vehicle access set, so the NEXT
// handshake (and every per-vehicle REST handler) re-reads it from the database.
// Consumer-site interface; satisfied by *auth.JWTAuthenticator.
//
// Nil in dev mode, where the NoopAuthenticator holds no cache to bust.
type accessSetInvalidator interface {
	InvalidateVehicles(userID string)
}

// accessSetWidener makes a user's ALREADY-OPEN sessions re-handshake, so an
// access set that grew is picked up now rather than at their next reconnect.
// Consumer-site interface; satisfied by *shareWidenBusNotifier.
//
// It is the second half and not a substitute for the first: the invalidator
// fixes the next handshake, this one makes a next handshake happen.
type accessSetWidener interface {
	ShareAccessWidened(userID, vehicleID, reason string)
}

// accessSetNarrower ends a user's live sessions when their access set SHRANK.
// Consumer-site interface; satisfied by *shareAccessBusNotifier.
//
// The provisioning path needs it for exactly one case — the MYR-599 owner-wins
// transfer, which takes a car away from the account that linked it first.
type accessSetNarrower interface {
	ShareAccessRevoked(userID, vehicleID, reason string)
}

// ownerStreamAccess is the access-set seam handed to the provisioning hook.
//
// One struct rather than three parameters because the three are always wired
// together and always from the same two objects.
//
// EVERY FIELD IS OPTIONAL, AND THE COMBINATIONS ARE NOT HYPOTHETICAL — this
// comment previously described a wiring that does not exist, so here is the one
// that does (main.go, ownerStreamAccessFrom):
//
//   - PRODUCTION: all three set. The invalidator is the *auth.JWTAuthenticator
//     that owns the access-set cache; the widener and narrower are the bus
//     notifiers whose events the hub's dispatchers consume.
//   - DEV MODE: the invalidator is nil and the widener and narrower are LIVE.
//     The bus notifiers are wired unconditionally — they need nothing from the
//     authenticator — while dev mode's NoopAuthenticator holds no cache to
//     bust. That is not the dangerous combination it looks like: a dev client
//     is authorized for every vehicle by the wildcard sentinel, so there is no
//     cached answer that could come back stale and no car that could be
//     missing from a re-handshake.
//   - ALL THREE NIL: the pre-MYR-601 behavior — the car appears at the cache
//     TTL lapse or at the 60-second revalidation sweep. This is what every test
//     that wires no bus gets.
//
// THE COMBINATION TO AVOID is a live widener next to a REAL cache that is not
// busted, which only a production deployment could produce: the widen provokes
// a reconnect, and a handshake served from the stale set comes back WITHOUT the
// car — a fix that looks like it ran. `ownerStreamAccessFrom` takes both from
// the same deps, so the two cannot be wired apart by accident; nothing enforces
// it beyond that.
type ownerStreamAccess struct {
	invalidator accessSetInvalidator
	widener     accessSetWidener
	narrower    accessSetNarrower
}

// gained announces that userID's access set now CONTAINS vehicleID.
//
// ORDER IS NORMATIVE AND IS THE WHOLE CORRECTNESS OF THIS FUNCTION: bust, then
// widen. The widen provokes a reconnect, and a handshake served from a stale
// cached set would come back without the car — a no-op that looks like a fix.
// It is the same order every narrowing path in internal/telemetry documents,
// for the mirror-image reason.
//
// Best-effort by construction, like every other step in this hook: a nil seam,
// an empty user or an empty vehicle are ordinary no-ops, and the link the
// caller is completing does not depend on anybody being online to hear this.
func (h *ownerStreamHook) gained(userID, vehicleID, reason string) {
	if userID == "" || vehicleID == "" {
		return
	}
	if h.access.invalidator != nil {
		h.access.invalidator.InvalidateVehicles(userID)
	}
	if h.access.widener != nil {
		h.access.widener.ShareAccessWidened(userID, vehicleID, reason)
	}
	h.logger.Info("owner stream setup: access set widened",
		slog.String("event", "owner_vehicle_access_widened"),
		slog.String("user_id", userID),
		slog.String("vehicle_id", vehicleID),
		slog.String("reason", reason))
}

// lost announces that userID's access set no longer contains vehicleID — the
// MYR-599 transfer's other end, and the only narrowing this hook can cause.
//
// SAME ORDER, DIFFERENT STAKES. A widening that arrives late costs somebody a
// car on their map; a narrowing that arrives late leaves a stranger streaming
// live GPS from a car that is no longer theirs in any sense, for up to the
// cache TTL and for as long as they hold the socket open. The former driver's
// grants were revoked inside the provisioning transaction, so the database
// agrees already — these two calls are what make the running process agree.
func (h *ownerStreamHook) lost(userID, vehicleID, reason string) {
	if userID == "" || vehicleID == "" {
		return
	}
	if h.access.invalidator != nil {
		h.access.invalidator.InvalidateVehicles(userID)
	}
	if h.access.narrower != nil {
		h.access.narrower.ShareAccessRevoked(userID, vehicleID, reason)
	}
	h.logger.Info("owner stream setup: access set narrowed",
		slog.String("event", "owner_vehicle_access_narrowed"),
		slog.String("user_id", userID),
		slog.String("vehicle_id", vehicleID),
		slog.String("reason", reason))
}

// announceProvisioned is the ONE call provisionVehicle makes once a car is on
// the row, and it decides whether anything is announced at all.
//
// NOT EVERY PROVISION WIDENS ANYTHING, and that is the point of the branch.
// AfterLink is a PASSIVE BULK SYNC — it runs over the owner's whole fleet on
// every link and every re-link — so announcing on every pass would re-handshake
// every session the owner holds each time they refresh their Tesla consent, for
// cars that have been in their access set for months. `Inserted` (Postgres's
// `xmax = 0`, carried out of the upsert for exactly this kind of question) is
// what separates a car ARRIVING from a car being reconciled.
//
// The transfer is the second door: the row is not inserted — it already existed
// under somebody else — but it has moved into this user's access set and out of
// the previous linker's and out of every grantee's, so ALL of those ends are
// announced.
//
// THE LOSSES GO FIRST, AND THE ORDER FOLLOWS THE STAKES. A widening that
// arrives late costs the arriving owner a car on their map for one reconnect; a
// narrowing that arrives late leaves a stranger streaming live GPS from a car
// that is no longer theirs in any sense. Both publishes are best-effort onto an
// in-process bus that drops the OLDEST event when a subscriber is behind, so if
// exactly one of them is going to be lost it must not be a security one. See
// `lost`.
func (h *ownerStreamHook) announceProvisioned(userID string, res store.VehicleUpsertResult) {
	switch {
	case res.Outcome == store.VehicleOwnedByTransfer:
		// The former driver, who was not asked and is not told anywhere else.
		h.lost(res.PreviousUserID, res.VehicleID, "superseded_by_owner")
		// AND EVERY THIRD PARTY THE SAME TRANSACTION CUT. The teardown revokes
		// every live grant on the car, not just the linker's claim, so the
		// driver's viewers lose access in the same statement — and theirs are
		// the sessions most likely to be open and watching. A transfer that
		// closed only the driver's socket would hand the arriving owner a car
		// whose live GPS was still streaming to strangers.
		for _, granteeID := range res.RevokedGranteeIDs {
			h.lost(granteeID, res.VehicleID, "share_superseded_by_owner")
		}
		h.gained(userID, res.VehicleID, "owner_transfer")
	case res.Inserted:
		h.gained(userID, res.VehicleID, "provisioned")
	}
}

// ownerStreamAccessFrom binds the seam to the objects main already built for
// the sharing surface: the same authenticator whose cache a redeem busts, and
// the same two bus notifiers whose events the hub's dispatchers consume.
//
// DELIBERATELY THE SAME OBJECTS AND THE SAME TOPICS. A provisioning is not a
// different kind of access change from a share redemption — it is the same
// change arriving through a different door — and giving it its own pipeline
// would mean two mechanisms to keep correct, which is the thing MYR-609's
// finding 4 asked the next producer not to do.
func ownerStreamAccessFrom(deps httpRouteDeps) ownerStreamAccess {
	return ownerStreamAccess{
		invalidator: deps.accessInvalidator,
		widener:     deps.shareAccessWidener,
		narrower:    deps.shareAccessNotifier,
	}
}
