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
func (h *ownerStreamHook) gained(userID, vehicleID, reason string) {
	var publish func(userID, vehicleID, reason string)
	if w := h.access.widener; w != nil {
		publish = w.ShareAccessWidened
	}
	h.announceAccessChange(userID, vehicleID, reason,
		"owner_vehicle_access_widened", "owner stream setup: access set widened", publish)
}

// lost announces that userID's access set no longer contains vehicleID — the
// MYR-599 transfer's other end, and the only narrowing this hook can cause.
//
// SAME ORDER, DIFFERENT STAKES. A widening that arrives late costs somebody a
// car on their map; a narrowing that arrives late leaves a stranger streaming
// live GPS from a car that is no longer theirs in any sense, for up to the
// cache TTL and for as long as they hold the socket open. That is also why
// announceProvisioned publishes the losses BEFORE the gain. The revocations
// happened inside the provisioning transaction, so the database agrees already
// — these calls are what make the running process agree.
func (h *ownerStreamHook) lost(userID, vehicleID, reason string) {
	var publish func(userID, vehicleID, reason string)
	if n := h.access.narrower; n != nil {
		publish = n.ShareAccessRevoked
	}
	h.announceAccessChange(userID, vehicleID, reason,
		"owner_vehicle_access_narrowed", "owner stream setup: access set narrowed", publish)
}

// announceAccessChange is the ONE body both directions share: bust the user's
// cached access set, hand the change to the seam, say so in the log.
//
// The two callers were byte-identical apart from the seam field and the two
// strings, which is the shape a rule takes just before one copy drifts — and
// the rule here is an ORDERING that a drifted copy would silently invert.
//
// BEST-EFFORT BY CONSTRUCTION, like every other step in this hook: an empty
// user, an empty vehicle or a nil seam are ordinary no-ops, and the link the
// caller is completing does not depend on anybody being online to hear this.
//
// AND IT SAYS NOTHING WHEN IT DID NOTHING. With no invalidator and no seam —
// the all-nil configuration every test that wires no bus gets — an
// `owner_vehicle_access_widened` line would be an audit trail asserting an
// announcement that never left the process.
func (h *ownerStreamHook) announceAccessChange(
	userID, vehicleID, reason, event, message string,
	publish func(userID, vehicleID, reason string),
) {
	if userID == "" || vehicleID == "" {
		return
	}
	acted := false
	if h.access.invalidator != nil {
		h.access.invalidator.InvalidateVehicles(userID)
		acted = true
	}
	if publish != nil {
		publish(userID, vehicleID, reason)
		acted = true
	}
	if !acted {
		return
	}
	h.logger.Info(message,
		slog.String("event", event),
		slog.String("user_id", userID),
		slog.String("vehicle_id", vehicleID),
		slog.String("reason", reason))
}

// accessGain is the widening one pass over a fleet produced, held until the
// pass is over.
//
// A FIRST LINK OF AN N-CAR FLEET IS ONE ACCESS-SET CHANGE, NOT N (MYR-601
// review round). Announcing per car closed every session that owner held N
// times — each close provoking a reconnect that raced the next close — to
// deliver one fact the first re-handshake had already delivered in full, since
// the reconnect re-derives the WHOLE access set. It is the same argument
// §7.5.5 redeem makes for publishing once per redemption rather than once per
// granted car, arriving from the other side.
//
// The vehicle id and reason are the FIRST gain of the pass and are for the log
// alone: `Hub.WidenUserAccess` cannot find a user's sessions by a car they are
// not yet authorized for — that is the whole reason it exists — so it closes
// every session the user holds whatever id it is handed.
type accessGain struct {
	vehicleID string
	reason    string
	found     bool
}

// record keeps the first gain of the pass. Later ones change nothing: one
// re-handshake already covers every car the pass provisioned.
func (g *accessGain) record(vehicleID, reason string) {
	if g.found {
		return
	}
	g.vehicleID, g.reason, g.found = vehicleID, reason, true
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
// THE GAIN IS COLLECTED, THE LOSSES ARE PUBLISHED NOW, and the asymmetry is the
// stakes. A gain deferred to the end of the pass costs the arriving user
// nothing — one re-handshake covers every car — while a loss deferred is a
// stranger streaming live GPS from a car that is no longer theirs in any sense
// for as long as the pass runs. Both ride an in-process bus that drops the
// OLDEST event when a subscriber is behind, so if exactly one is going to be
// lost it must not be the security one. See `lost`.
//
// IT NO LONGER TAKES THE GAINING USER. Everything it publishes is addressed at
// somebody ELSE — the accounts the transfer cut — and the one fact about the
// caller travels in `gain`, to be announced once by flushGain when the pass is
// over.
func (h *ownerStreamHook) announceProvisioned(res store.VehicleUpsertResult, gain *accessGain) {
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
		gain.record(res.VehicleID, "owner_transfer")
	case res.Inserted:
		gain.record(res.VehicleID, "provisioned")
	}
}

// flushGain announces at most ONE widening for a whole pass. A pass that
// provisioned nothing new says nothing at all.
func (h *ownerStreamHook) flushGain(userID string, gain accessGain) {
	if !gain.found {
		return
	}
	h.gained(userID, gain.vehicleID, gain.reason)
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
