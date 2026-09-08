package telemetry

import (
	"context"
	"time"
)

// The consumer-site SHAPES and INTERFACES for the MYR-602 trips surface
// (rest-api.md §7.30).
//
// internal/telemetry never imports internal/store (the dependency rule), so
// these mirror the store's types and an adapter in cmd/ translates. The
// duplication is the price of the boundary and it is paid deliberately: it is
// what lets the handler tests run with a hand-written fake and no database.

// TripData is one trip as the handler renders it. Field-for-field the store's
// TripView, minus the internal columns no wire consumer has a use for (the
// sweeper's notification stamps) and minus each participant's user id, which
// is collapsed into `userIsSelf` before it can reach a response.
type TripData struct {
	ID        string
	VehicleID string

	// Name is PLAINTEXT and P1 user content. Never logged, never placed in an
	// error message.
	Name string

	StartsAt  time.Time
	EndsAt    time.Time
	EndedAt   *time.Time
	CreatedAt time.Time

	// Role is the caller's relationship — `owner` or `participant`, the
	// contract's TripRole enum. Resolved in the store, in the same statement
	// that read the row.
	Role string

	// OwnerFirstName is the trip owner's confirmed first name, or nil. Same
	// three-rung ladder and same confirmation gate as
	// VehicleSummary.ownerFirstName.
	OwnerFirstName *string

	Vehicle TripVehicleData

	// Participants is the LIVE roster. Everyone on the trip sees the whole
	// list; they are on a trip together.
	Participants []TripParticipantData

	DriveCount int

	// TotalDistanceMiles and TotalDurationMinutes are the window's drives
	// summed (MYR-608), or nil when the window holds none. Same window, same
	// read and same statement as DriveCount, so the three numbers on a card
	// cannot describe three different sets of drives.
	//
	// RUNNING TOTALS: an active trip reports what it has driven so far, and the
	// numbers climb between reads. The client decides how to render that; the
	// server does not withhold a total for being provisional.
	//
	// MINUTES HERE, SECONDS ON THE WIRE — converted at the same boundary
	// `DriveSummary.durationSeconds` is converted at, because the underlying
	// column is minutes and the contract is seconds.
	TotalDistanceMiles   *float64
	TotalDurationMinutes *int64

	// TotalEnergyKwh is the same window's energy consumption summed (MYR-629),
	// read in the SAME statement as the three above. The client divides it by
	// TotalDistanceMiles to render Wh/mi; the server stores no ratio, because a
	// stored efficiency could disagree with the two numbers it was derived from.
	//
	// ITS NIL RULE IS STRICTER THAN ITS SIBLINGS'. `Drive."energyUsedKwh"` is
	// NOT NULL, so a drive the tracker could not measure writes 0 rather than
	// null, and a plain SUM would mix those zeros into a real total — producing
	// a Wh/mi computed from some drives' energy over ALL drives' miles. So the
	// total is ALL-OR-NOTHING over the drives that moved: nil when ANY drive in
	// the window covered distance without reporting energy, and nil when the
	// window holds no drives at all. queryTripDriveTotals carries the full
	// argument, including why this is also the migration rule for the windows
	// that straddle the MYR-629 fix.
	TotalEnergyKwh *float64

	// CurrentLeg is the driving leg underway, or nil. INFORMATIONAL, NEVER A
	// GATE — a consumer must not condition its map, its live screen or its
	// socket subscription on this being present. An active trip with no leg is
	// the ordinary overnight state.
	CurrentLeg *TripLegData
}

// TripVehicleData is the catalog subset carried on the trip so a participant's
// card needs no second call.
//
// The RAW identity columns. `vinLast4` and the resolved `trimLabel` are
// produced at the wire layer by the SAME helpers §7.0 and §7.1 use, so the
// three surfaces cannot name one car three different ways.
type TripVehicleData struct {
	VehicleID string
	Name      string
	Model     string
	Year      int
	Color     string
	VIN       string
	TrimLabel *string
	Trim      *string
}

// TripParticipantData is one roster entry.
type TripParticipantData struct {
	// ParticipantID is the SHARE id — the wire contract's `participantId`, so
	// the roster round-trips through PATCH without a second identifier.
	ParticipantID string

	// Name is the roster display name: the accepting account's confirmed FIRST
	// name, else the owner's own label for the grant. Never a full name.
	Name string

	// UserID is INTERNAL and never reaches the wire. The wire layer compares
	// it against the caller to produce `userIsSelf` and then drops it —
	// publishing it would hand every participant a durable identifier for
	// everybody else on the trip.
	UserID string

	// AddedByName is the confirmed FIRST name of whoever put this person on the
	// trip (MYR-618), or nil. Nil covers a row written before the column
	// existed, an adder who has not been through the naming prompt, and an
	// adder whose account is gone — one value for three causes, deliberately,
	// because they mean the same thing to the person reading the roster.
	AddedByName *string
}

// TripAddablePersonData is one row of §7.30.11 — somebody who already holds a
// live grant on the trip's vehicle and is not yet on the trip.
//
// TWO FIELDS, and the narrowness is the contract. This read is admitted to
// PARTICIPANTS, not only the owner, so it must not become §7.5's grant listing
// reached through a trip: no invite codes, no email addresses, no statuses, no
// permissions.
type TripAddablePersonData struct {
	// ShareID is the grant's id — the same value the roster calls
	// `participantId` and PATCH calls `addParticipantIds`, so a picked person
	// round-trips into the add with no translation.
	ShareID string

	// Name is the accepting account's confirmed FIRST name, else the owner's
	// own label for the grant. Same ladder and same fallback as the roster.
	Name string
}

// TripLegData is the open leg as the card renders it.
type TripLegData struct {
	DestinationName string
	// EtaMinutes is read at REST-read time from the vehicle's live navigation,
	// not frozen at leg start. Nil when navigation reports none.
	EtaMinutes *int
	StartedAt  time.Time
}

// TripCreateInput is the decoded POST body plus the resolved owner.
type TripCreateInput struct {
	VehicleID           string
	OwnerUserID         string
	Name                string
	StartsAt            time.Time
	EndsAt              time.Time
	ParticipantShareIDs []string
}

// TripUpdateInput is the decoded PATCH body. nil means UNCHANGED — the
// distinction the wire draws between an absent key and a null one survives to
// this layer rather than being flattened into a zero value.
type TripUpdateInput struct {
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

// TripStore is everything the trips handler needs from persistence.
//
// ONE INTERFACE RATHER THAN SIX, against the usual "interfaces should be
// small" instinct, because every method is served by the same aggregate and
// every one of them re-resolves the caller's relationship to the trip itself.
// Splitting them would invite a wiring in which four of the six are present —
// and the missing two would be access checks.
//
// EVERY METHOD TAKES THE CALLER'S userID AND ENFORCES ACCESS ITSELF. The
// handler's checks produce good error messages; these are what actually decide.
// A method that accepted a pre-authorized flag would be one refactor away from
// being called with it set.
type TripStore interface {
	// CreateTrip opens a window. The caller has already been established as
	// the vehicle's owner; the store validates the window, the overlap and the
	// participants.
	CreateTrip(ctx context.Context, in TripCreateInput) (TripData, error)

	// GetTrip returns one trip for a caller who owns it or is a live
	// participant. ErrTripNotFound for everybody else, and for an unknown id.
	GetTrip(ctx context.Context, tripID, userID string) (TripData, error)

	// ListTrips returns the caller's trips, newest first. status is the empty
	// string for "no filter".
	ListTrips(ctx context.Context, userID, status string, limit int) ([]TripData, error)

	// UpdateTrip applies a patch. Owner-only; ErrTripNotFound to anybody else.
	UpdateTrip(ctx context.Context, tripID, ownerUserID string, in TripUpdateInput) (TripData, error)

	// AddTripParticipants widens the roster on behalf of the OWNER OR A LIVE
	// PARTICIPANT (MYR-618, §7.30.4).
	//
	// A SEPARATE METHOD FROM UpdateTrip, and the separation is the whole
	// safety argument rather than a naming preference: UpdateTrip can rename a
	// trip, move its window and REMOVE people, and all three stay owner-only.
	// This one can only add, because adding is the only thing its
	// implementation contains — so a handler bug that reached it with a
	// participant's id cannot become a removal.
	//
	// The share ids must resolve to accepted, unsuspended grants on the trip's
	// vehicle (ErrTripParticipantNotShared otherwise, all-or-nothing), the trip
	// must not have ended (ErrTripEnded), and a caller who is neither owner nor
	// live participant receives ErrTripNotFound like everybody else.
	//
	// Adding somebody already on the trip is a NO-OP: it writes nothing,
	// audits nothing and announces nothing, and the call still succeeds.
	AddTripParticipants(ctx context.Context, tripID, actorUserID string, shareIDs []string) (TripData, error)

	// TripAddablePeople is §7.30.11: the trip vehicle's accepted, unsuspended
	// grant-holders who are not already on the trip, for a caller who owns it
	// or is on it. ErrTripNotFound for everybody else — the same answer an
	// unknown trip id gets.
	TripAddablePeople(ctx context.Context, tripID, userID string) ([]TripAddablePersonData, error)

	// EndTrip stamps the early end. IDEMPOTENT: a second call returns the
	// already-ended trip rather than moving the end forward.
	EndTrip(ctx context.Context, tripID, ownerUserID string) (TripData, error)

	// LeaveTrip marks the caller's own membership left. Idempotent and silent
	// — it reports nothing about whether the trip exists.
	LeaveTrip(ctx context.Context, tripID, userID string) error

	// DeleteTrip removes the trip and its three children in one transaction,
	// and writes the `trip.deleted` audit row inside it (MYR-607, §7.30.10).
	// OWNER-ONLY: ErrTripNotFound for a participant, a stranger and an unknown
	// id alike, so the most destructive route on this surface tells a caller
	// nothing the read routes would not.
	//
	// The vehicle's DRIVES are untouched — a trip never owned one.
	DeleteTrip(ctx context.Context, tripID, ownerUserID string) error

	// TripDrives lists the window's drives for a caller who owns the trip or
	// is a live participant.
	TripDrives(ctx context.Context, tripID, userID string, cursor DriveListCursor, limit int) (DriveListPage, error)

	// RegisterTripActivityStartToken stores an ActivityKit PUSH-TO-START
	// token. The token is a P1 CAPABILITY: implementations must never log it
	// beyond a short prefix, never echo it, and never place it in an error.
	RegisterTripActivityStartToken(ctx context.Context, tripID, userID, token string, sandbox bool) error

	// DeleteTripActivityStartToken removes it. Idempotent.
	DeleteTripActivityStartToken(ctx context.Context, tripID, userID string) error

	// TripLegAccess resolves the caller's standing on ONE LEG (§7.21.7): which
	// trip it belongs to, and whether the leg is still open.
	//
	// The route carries no trip id, so this is where the authorization comes
	// from. ErrTripNotFound covers BOTH "no such leg" and "not your trip" —
	// one answer, so the endpoint cannot be used to discover leg ids — while
	// `open=false` is a different refusal to a genuine member and must stay
	// distinguishable from it.
	TripLegAccess(ctx context.Context, legID, userID string) (tripID string, open bool, err error)

	// RegisterTripLegActivityToken files the per-ACTIVITY update token for one
	// running card (§7.21.7). A DIFFERENT TOKEN AND A DIFFERENT TABLE from the
	// two above: those address the APP's capability to create a card, this
	// addresses one card that already exists.
	//
	// The tripID comes from TripLegAccess, never from the caller, and the
	// implementation re-asserts it alongside the open-leg guard so a leg that
	// closed between the probe and the write is refused by the statement.
	//
	// Also a P1 CAPABILITY: never logged beyond a short prefix, never echoed,
	// never in an error.
	RegisterTripLegActivityToken(ctx context.Context, tripID, legID, userID, token string, sandbox bool) error

	// EndTripLegActivityToken tombstones the caller's own card on a leg,
	// reporting whether a live row matched (§7.21.7). Caller-scoped so nobody
	// can end another person's card, and idempotent: a miss is not an error.
	EndTripLegActivityToken(ctx context.Context, legID, userID string) (bool, error)
}
