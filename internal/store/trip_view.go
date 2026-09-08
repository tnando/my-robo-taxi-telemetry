package store

import "time"

// The COMPOSED READ SHAPES for MYR-602 trips — what one GET returns, as
// opposed to what one table holds (trip_types.go).
//
// They are separate types rather than fields on Trip because they are assembled
// from five relations and three of them are OPTIONAL decorations: a list of
// twenty trips does not read twenty rosters. Keeping the row and the view apart
// is what lets the repository choose, per call, how much to assemble.

// TripView is one trip as a caller sees it: the row, the caller's relationship
// to it, and the decorations the trip card renders.
type TripView struct {
	Trip

	// Role is the CALLER's relationship — `owner` or `participant`, the
	// contract's TripRole enum. Resolved in the same statement that reads the
	// row, so there is no read-then-authorize window. A caller who is neither
	// never receives a TripView; the read returns ErrTripNotFound.
	Role string

	// OwnerFirstName is the trip owner's confirmed first name, or nil. Walks
	// the same three-rung ladder behind VehicleSummary.ownerFirstName and is
	// gated on the same confirmation row, so a person who has not yet been
	// through the naming prompt is nil here exactly as they are there.
	OwnerFirstName *string

	Vehicle TripVehicle

	// Participants is the LIVE roster. Everyone sees the whole list — they are
	// on a trip together, and a road trip whose members cannot see each other
	// is a group chat with the names blanked.
	Participants []TripParticipantView

	// DriveCount is how many drives fall in the window right now. Counted at
	// read time, like everything else about a window-scoped feature: a trip
	// created over a week already driven has a non-zero count the instant it
	// exists, with nothing to backfill.
	DriveCount int

	// CurrentLeg is the driving leg underway, or nil. INFORMATIONAL, NEVER A
	// GATE — access is purely the window, so an active trip with no leg is the
	// ordinary overnight state and not a degraded one.
	CurrentLeg *TripLegView
}

// TripVehicle is the catalog subset the trip card renders, carried on the trip
// so a participant's card needs no second call.
//
// The RAW identity columns, not the resolved wire values: `vinLast4` and the
// resolved `trimLabel` are produced at the wire layer by the SAME helpers the
// catalog and the snapshot use (internal/trim.Resolve, lastFourOfVIN), so the
// three surfaces cannot name one car three different ways.
type TripVehicle struct {
	VehicleID string
	Name      string
	Model     string
	Year      int
	Color     string
	VIN       string

	// TrimLabel and Trim are Tesla's display-safe label and the raw badge code.
	// Both feed the resolver; neither is the answer on its own.
	TrimLabel *string
	Trim      *string
}

// TripParticipantView is one roster entry.
type TripParticipantView struct {
	// ParticipantID is the SHARE id, which is what the wire contract calls
	// `participantId`. The roster round-trips through PATCH's
	// add/removeParticipantIds without the client holding a second identifier.
	ParticipantID string

	// UserID is internal. It never reaches the wire — the handler compares it
	// against the caller to set `userIsSelf` and then drops it. Publishing
	// another person's user id on a roster would hand every participant a
	// durable identifier for everybody else on the trip.
	UserID string

	// Name is the roster display name: the accepting account's CONFIRMED first
	// name if there is one, else the owner's own label for the grant. Never a
	// full name — first names only is the P1 policy for anything delivered to
	// a counterparty.
	Name string

	// AddedByName is the confirmed FIRST name of whoever put this person on the
	// trip (MYR-618), or nil.
	//
	// NIL HAS THREE CAUSES AND THEY ARE DELIBERATELY ONE VALUE: the row predates
	// migration 0060 and records no adder; the adder has not been through the
	// naming prompt (the MYR-583 confirmation gate); or the adder's account is
	// gone. All three mean the same thing to the person reading the roster —
	// there is no name to show — and distinguishing them on the wire would
	// disclose more about a third party than the row is entitled to.
	AddedByName *string
}

// TripAddablePersonView is one row of §7.30.11: somebody who already holds a
// live grant on the trip's vehicle and is not yet on the trip.
//
// TWO FIELDS AND NO MORE, and the narrowness is the contract rather than an
// economy. This read is admitted to PARTICIPANTS, not just the owner, and §7.5's
// grant listing — invite codes, statuses, permissions, the owner's private memo
// on why somebody has a key — stays owner-only for reasons MYR-618 did not
// change. A picker needs a name to show and an id to post back; anything else
// would be a share surface reached through a trip.
type TripAddablePersonView struct {
	// ShareID is the grant's id — the same value §7.30.4 calls
	// `addParticipantIds` and the roster calls `participantId`, so a person
	// picked here round-trips into the add with no translation.
	ShareID string

	// DisplayName is the accepting account's CONFIRMED first name, else the
	// owner's own label for the grant. Same ladder, same fallback and same
	// first-name-only policy as the roster, so a person cannot be called one
	// thing in the picker and another thing on the trip they were just added to.
	DisplayName string
}

// TripLegView is the open leg as the trip card renders it.
type TripLegView struct {
	// DestinationName is the PLAINTEXT place the car said it was driving to.
	// P1 — sealed at rest, decrypted at this boundary, never logged.
	DestinationName string

	// EtaMinutes is read from the VEHICLE row, not the leg row, and therefore
	// at REST-READ TIME: the leg records what the car set off toward, the
	// vehicle carries how far away it is now. Nil when navigation is not
	// reporting one.
	EtaMinutes *int

	StartedAt time.Time
}

// CreateTripInput is what POST /api/vehicles/{id}/trips supplies.
type CreateTripInput struct {
	VehicleID   string
	OwnerUserID string

	// Name is PLAINTEXT and P1. Validated (1..MaxTripNameLen runes after
	// trimming) before it reaches the repository, and sealed on the way in.
	Name string

	StartsAt time.Time
	EndsAt   time.Time

	// ParticipantShareIDs are SHARE ids, not user ids. Every one must resolve
	// to a live accepted grant on THIS vehicle or the whole create fails with
	// ErrTripParticipantNotShared — all or nothing, because a create that
	// silently dropped one invitee would produce a trip the owner believes has
	// four people on it and that has three.
	ParticipantShareIDs []string
}

// UpdateTripInput is PATCH's body, already decoded. Every field is optional and
// nil means UNCHANGED — the distinction the wire draws between "absent" and
// "null" survives to this layer rather than being flattened into a zero value.
type UpdateTripInput struct {
	Name   *string
	EndsAt *time.Time
	// AddParticipantIDs and RemoveParticipantIDs are SHARE IDS, not user ids —
	// the same values ParticipantShareIDs carries on create, named more tersely
	// only because the patch verb is already in the field name.
	//
	// THE NAMING ASYMMETRY IS WORTH ONE COMMENT rather than a rename, because
	// the wire keys are `addParticipantIds` / `removeParticipantIds` and a
	// rename here would leave the Go field and the JSON key disagreeing. What
	// they mean is fixed by the model: a trip creates NO new vehicle
	// relationship — participants are chosen from the car's already-accepted
	// grants — so a share id is the only identifier that can express "this
	// person, on this car", and a user id would name somebody without saying
	// which grant admits them.
	AddParticipantIDs    []string
	RemoveParticipantIDs []string
}
