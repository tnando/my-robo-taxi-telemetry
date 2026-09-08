package store

import (
	"errors"
	"time"
)

// MYR-602 TRIPS — the domain types behind go_trips, go_trip_participants,
// go_trip_activity_tokens and go_trip_legs (migration 0047).
//
// A trip is a (vehicle, window, participant set) tuple owned by the vehicle's
// owner. It creates no new vehicle relationship: participants are picked from
// the car's ACCEPTED share grants, and the trip decides only what that existing
// grant means between two instants. See docs/architecture/trips.md.

// TripStatus is the derived lifecycle of a trip. It is NOT a stored column —
// it is a function of the window and the clock, computed on every read.
//
// Storing it would create the one state the platform could not explain: a row
// that says `active` because a sweeper pass was missed, on a window that closed
// an hour ago. The sweeper's stamps record what was NOTIFIED, never what is
// TRUE; truth is always recomputed from the instants.
type TripStatus string

const (
	// TripStatusScheduled — now < startsAt. The participants have been told
	// (trip_added) but hold no live access yet.
	TripStatusScheduled TripStatus = "scheduled"

	// TripStatusActive — startsAt <= now < effectiveEnd. This is the only
	// status that widens anybody's access set.
	TripStatusActive TripStatus = "active"

	// TripStatusEnded — now >= effectiveEnd, whether the window ran out or the
	// owner ended it early.
	TripStatusEnded TripStatus = "ended"
)

// Trip is one owner-defined window on one vehicle.
type Trip struct {
	ID          string
	VehicleID   string
	OwnerUserID string

	// Name is the PLAINTEXT trip name. P1 user content: it is sealed as
	// name_enc at rest and decrypted at this boundary, exactly like a ride's
	// place labels. Never log it.
	Name string

	StartsAt time.Time
	EndsAt   time.Time

	// EndedAt is non-nil once the owner has ended the trip early.
	EndedAt *time.Time

	// StartedNotifiedAt / EndedNotifiedAt are the trip sweeper's idempotency
	// stamps, not lifecycle facts. A trip whose window opened while the server
	// was down is ACTIVE the moment it comes back, with StartedNotifiedAt still
	// NULL — the sweeper then sends the push it owes and stamps.
	StartedNotifiedAt *time.Time
	EndedNotifiedAt   *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// EffectiveEnd is min(EndsAt, EndedAt) — the instant access actually stops.
//
// Computed rather than written back over EndsAt because the owner's stated
// window is a separate fact from the early end, and collapsing the two would
// make an accidental "End trip" unexplainable afterwards.
func (t Trip) EffectiveEnd() time.Time {
	if t.EndedAt != nil && t.EndedAt.Before(t.EndsAt) {
		return *t.EndedAt
	}
	return t.EndsAt
}

// Ended reports whether the OWNER has ended this trip.
//
// A NON-NIL EndedAt IS TERMINAL ON ITS OWN, with no comparison against the
// clock, and that is a statement about what the column MEANS rather than a
// shortcut. `ends_at` is a scheduled instant that may lie in the future;
// `ended_at` records that an action ALREADY HAPPENED, and its only writer is
// `SET ended_at = NOW()` inside queryEndTrip. It can therefore never hold a
// future instant, so there is nothing for a comparison to decide.
//
// STATING IT THIS WAY REMOVES A CROSS-MACHINE CLOCK-SKEW BUG, and it is a real
// one rather than a hypothetical: the instant is written by POSTGRES and the
// status is derived in GO, on a different machine. A test against a
// testcontainers Postgres measured the container's clock 76 ms AHEAD of the
// host's — so for 76 ms after an owner tapped "End trip", `now.Before(*EndedAt)`
// was true and the response to their own request said the trip was still
// ACTIVE. The window predicate in SQL never had the problem (it compares NOW()
// against a column, both on the database's clock); only the Go derivation
// spanned two clocks, and only in the direction that reports a closed window as
// open.
func (t Trip) Ended() bool { return t.EndedAt != nil }

// StatusAt derives the trip's status at an instant.
//
// The boundaries are [startsAt, effectiveEnd) — start is inclusive, end is
// exclusive. That is the same convention the access query's SQL predicate uses
// (`starts_at <= now AND now < COALESCE(...)`), and the two MUST agree: a trip
// this function calls `active` while the access query calls it over would
// render a live card over a socket that has already dropped the vehicle.
func (t Trip) StatusAt(now time.Time) TripStatus {
	switch {
	// THE OWNER'S EARLY END IS CHECKED FIRST and is not compared against the
	// clock at all — see Ended() for why the column's meaning, not a
	// comparison, is what settles it, and for the 76 ms of clock skew that
	// made the comparison report a closed window as open.
	//
	// It also correctly ends a trip the owner cancelled while it was still
	// SCHEDULED, which the ordering below would otherwise have reported as
	// scheduled forever.
	case t.Ended():
		return TripStatusEnded
	case now.Before(t.StartsAt):
		return TripStatusScheduled
	case now.Before(t.EffectiveEnd()):
		return TripStatusActive
	default:
		return TripStatusEnded
	}
}

// TripParticipant is one person's membership of one trip.
type TripParticipant struct {
	TripID string
	UserID string

	// ShareID is the accepted go_vehicle_shares row the person was picked
	// from. It is the wire contract's `participantId`, so the roster
	// round-trips without the client holding a second identifier — but it is
	// NOT the access check. The access query re-joins the share by
	// (vehicle, user) and re-tests status and suspension.
	ShareID string

	AddedAt time.Time

	// LeftAt is a tombstone, set when the participant leaves or the owner
	// removes them. Non-nil means no access, immediately.
	LeftAt *time.Time
}

// Live reports whether this membership currently grants anything.
func (p TripParticipant) Live() bool { return p.LeftAt == nil }

// TripLeg is one driving leg inside a trip's window: the car set off with a
// destination and either arrived or stopped.
type TripLeg struct {
	ID        string
	TripID    string
	VehicleID string

	// DestinationName is the PLAINTEXT place the car said it was driving to
	// when the leg began. P1 (data-classification.md §1.18) — sealed at rest,
	// decrypted here, never logged.
	DestinationName string

	StartedAt time.Time
	EndedAt   *time.Time

	// Arrived is true ONLY on arrival evidence (the internal/arrival detector's
	// 80 m / 20 s dwell). A leg that ended because the car parked short, the
	// destination cleared, or the window closed is false — and the difference
	// decides both whether `trip_leg_arrived` fires and whether the Live
	// Activity's final content-state says `arrived` or `completed`.
	Arrived bool

	StartedNotifiedAt *time.Time
	ArrivedNotifiedAt *time.Time
	ActivityStartedAt *time.Time
	ActivityEndedAt   *time.Time

	CreatedAt time.Time
}

// Open reports whether the leg is still underway.
func (l TripLeg) Open() bool { return l.EndedAt == nil }

// TripActivityToken is one party's ActivityKit PUSH-TO-START token for one
// trip. The owner may hold one too — the owner is included in the per-leg
// Activity by explicit product decision.
type TripActivityToken struct {
	TripID string
	UserID string

	// PushToStartToken is P1 and a CAPABILITY: whoever holds it together with
	// the team's APNs signing key can start a Live Activity on that phone.
	// Never logged beyond the 8-character prefix internal/push's tokenPrefix
	// produces, never echoed into a response, never placed in an error
	// message. (internal/store does not import internal/push — the redaction
	// happens on the sending side, which is the only side that holds the
	// value at log time.)
	PushToStartToken string

	Sandbox bool
}

// MaxTripWindow is the ceiling on a trip's duration, mirrored by the
// go_trips_window_capped CHECK constraint. A trip is a standing live-location
// grant, so the cap is what stops a mistyped year handing out a decade of it.
const MaxTripWindow = 30 * 24 * time.Hour

// MaxTripNameLen is the trimmed rune ceiling on a trip name (contracts
// v0.41.0: 1..60). Runes, not bytes — a 60-character name in any script must
// be accepted, and a byte cap would silently reject one.
const MaxTripNameLen = 60

// Domain errors. Each maps to exactly one wire outcome at the handler layer;
// see internal/telemetry/trip_errors.go for the mapping.
var (
	// ErrTripNotFound is returned when no trip has the given id. Handlers
	// answer 404 — and they answer 404 for a trip the caller is simply not a
	// member of too, so the endpoint is never an oracle for trip ids.
	ErrTripNotFound = errors.New("store: trip not found")

	// ErrTripOverlap reports that another scheduled-or-active trip on the same
	// vehicle already covers part of the requested window. 409 trip_overlaps.
	ErrTripOverlap = errors.New("store: trip window overlaps an existing trip")

	// ErrTripWindowInvalid reports a window that is empty, inverted, or longer
	// than MaxTripWindow. 400.
	ErrTripWindowInvalid = errors.New("store: invalid trip window")

	// ErrTripNameInvalid reports a name that is empty after trimming, longer
	// than MaxTripNameLen RUNES, or carrying a control character. 400.
	//
	// Separate from ErrTripWindowInvalid rather than one "invalid input"
	// sentinel, because the two produce different sub-codes and because a
	// handler that could not tell them apart would have to report both
	// possibilities in one message.
	ErrTripNameInvalid = errors.New("store: invalid trip name")

	// ErrTripEnded reports a mutation attempted on a trip whose window has
	// already closed. 409.
	//
	// PATCH refuses on an ended trip rather than partially applying: every
	// mutation it offers is about a live window, and extending `ends_at` past
	// NOW() on a lapsed trip would RESURRECT live access that every
	// participant was already told had ended. Continuing a road trip is a new
	// trip, which says the true thing on everybody's phone.
	ErrTripEnded = errors.New("store: trip has already ended")

	// ErrTripParticipantNotShared reports a requested participant who does not
	// hold a live accepted share on THIS vehicle. 400 participant_not_shared.
	// It is deliberately the same answer for "no such share", "share on a
	// different car", "not accepted" and "suspended": the create endpoint must
	// not report which.
	ErrTripParticipantNotShared = errors.New("store: participant holds no accepted share on this vehicle")

	// ErrTripParticipantOwnerRemoved reports that a PARTICIPANT tried to add
	// somebody the trip's OWNER had removed. 409 conflict /
	// participant_owner_removed (MYR-618 review round, migration 0061).
	//
	// A `conflict` rather than a `permission_denied`, because the caller holds
	// the verb — a participant may add people — and it is this particular
	// person who may not be added, by a decision that already happened. It is
	// also not `participant_not_shared`: that answer says "get a share first",
	// which would be wrong advice here, since the person very likely still
	// holds one and the owner is the only remedy.
	//
	// AN OWNER NEVER SEES IT. Their add is what clears the marker.
	ErrTripParticipantOwnerRemoved = errors.New("store: the trip's owner removed this person")

	// ErrTripLegOpen reports that the trip already has an open leg. The leg
	// detector treats it as a no-op, not a failure — it is the idempotency
	// guard doing its job on a redelivered drive-start.
	ErrTripLegOpen = errors.New("store: trip already has an open leg")
)
