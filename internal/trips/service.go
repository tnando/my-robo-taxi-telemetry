package trips

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/push"
)

// The seams. Every one is declared HERE at the consumer site, so this package
// imports internal/push (for the two notification value types) and nothing
// else — never internal/store, never internal/ws. The cmd/ wiring adapts the
// repositories and the revalidator onto them.

// TripStore is the window half: who is on a trip, and which trips have crossed
// an edge.
type TripStore interface {
	TripAudienceFor(ctx context.Context, tripID string) (TripAudience, error)
	// TripNameFor decrypts the trip's name for the Live Activity's content
	// state. P1 user content — it reaches the card and nothing else, never a
	// log line and never an alert body.
	TripNameFor(ctx context.Context, tripID string) (string, error)
	ClaimTripsToStart(ctx context.Context, limit int) ([]string, error)
	ClaimTripsToEnd(ctx context.Context, limit int) ([]string, error)
	ClaimTripStartNow(ctx context.Context, tripID string) (bool, error)
	ClaimTripEndNow(ctx context.Context, tripID string) (bool, error)
	ActiveTripVehicles(ctx context.Context, limit int) ([]TripVehicle, error)
	// ActiveTripForVehicle re-asks, for ONE car, the question the candidate
	// snapshot answered in bulk: which open window is it inside right now,
	// or "" for none. It is the confirmation openLeg runs before it writes.
	ActiveTripForVehicle(ctx context.Context, vehicleID string) (string, error)
}

// TripAudience is who a trip's notifications go to. Mirrors store.TripAudience;
// the owner is separate from the participants because the two audiences differ
// per event — the lifecycle pushes go to participants, the LEG pushes go to the
// owner as well.
type TripAudience struct {
	TripID             string
	VehicleID          string
	OwnerUserID        string
	ParticipantUserIDs []string
}

// everyone returns the owner and the participants, for the leg fan-outs.
// Duplicates are tolerated: push.NotifyTrip de-duplicates recipients.
func (a TripAudience) everyone() []string {
	out := make([]string, 0, len(a.ParticipantUserIDs)+1)
	if a.OwnerUserID != "" {
		out = append(out, a.OwnerUserID)
	}
	return append(out, a.ParticipantUserIDs...)
}

// TripVehicle pairs a car with the open window it is inside.
type TripVehicle struct {
	VehicleID string
	TripID    string
}

// LegStore is the leg half.
type LegStore interface {
	StartLeg(ctx context.Context, tripID, vehicleID, destination string, startedAt time.Time) (Leg, error)
	// ResumeRecentLeg re-opens the leg this car just closed WITHOUT ARRIVING,
	// when it has set off again for the SAME place since notBefore. Reports
	// false — never an error — for every ordinary reason not to, because the
	// caller's next move on false is StartLeg. See openLeg.
	ResumeRecentLeg(ctx context.Context, vehicleID, destination string, notBefore time.Time) (Leg, bool, error)
	EndLeg(ctx context.Context, legID string, endedAt time.Time, arrived bool) error
	OpenLegForVehicle(ctx context.Context, vehicleID string) (Leg, error)
	OpenLegsForTrip(ctx context.Context, tripID string) ([]Leg, error)
	ClaimLegStartedPush(ctx context.Context, legID string) (bool, error)
	ClaimLegArrivedPush(ctx context.Context, legID string) (bool, error)
	ClaimLegActivityStart(ctx context.Context, legID string) (bool, error)
	ClaimLegActivityEnd(ctx context.Context, legID string) (bool, error)
}

// Leg mirrors store.TripLeg, narrowed to what this package reads.
type Leg struct {
	ID              string
	TripID          string
	VehicleID       string
	DestinationName string
	StartedAt       time.Time
	EndedAt         *time.Time
}

// Open reports whether the leg is still underway. A zero Leg is not open, which
// is how "this car has no leg" is expressed without a second return value.
func (l Leg) Open() bool { return l.ID != "" && l.EndedAt == nil }

// Pusher is the banner half of the notification surface.
type Pusher interface {
	NotifyTrip(ctx context.Context, p push.TripPush)
}

// ActivityPusher is the Live Activity half.
type ActivityPusher interface {
	StartLeg(ctx context.Context, tc push.TripLegContext) int
	// StartLegForUser raises ONE person's card for a leg that is ALREADY OPEN
	// — the MYR-612 catch-up for a phone whose token registered after the
	// fan-out had already run. It shares the fan-out's per-(device, leg) claim,
	// so the two cannot raise two cards for one journey.
	StartLegForUser(ctx context.Context, tc push.TripLegContext, userID string) int
	UpdateLeg(ctx context.Context, tc push.TripLegContext) int
	EndLeg(ctx context.Context, tc push.TripLegContext)
}

// Revalidator re-derives connected sockets' access and roles. Satisfied by
// *ws.AccessRevalidator.
//
// IT IS NUDGED ON EVERY TRANSITION rather than left to its own ticker, and the
// nudge is what makes a window edge feel instant instead of taking up to two
// intervals. Both run at 60 seconds, so unnudged, a trip opening a moment after
// a sweep would leave every participant's socket masked as a plain viewer for
// nearly a minute AFTER their phone buzzed to say the trip had started — the
// push and the map disagreeing is the one failure a person actually notices.
type Revalidator interface {
	SweepOnce(ctx context.Context) int
}

// VehicleNamer resolves a vehicle's nickname for the Live Activity's card.
type VehicleNamer interface {
	VehicleName(ctx context.Context, vehicleID string) (string, error)
}

// Service is the shared half of the sweeper and the leg detector: the stores,
// the two notification surfaces, and the transitions both of them make.
//
// The two components are separate types (Sweeper, Detector) over one Service
// because they have entirely different clocks — one is a ticker, the other is a
// telemetry frame — while the WORK at a transition is identical whichever of
// them noticed it. `SettleTrip` is the proof: the owner's early-end handler,
// the sweeper's closing edge and a window that elapsed while the server was
// down all end a trip through exactly one function.
type Service struct {
	trips      TripStore
	legs       LegStore
	pusher     Pusher
	activities ActivityPusher
	revalidate Revalidator
	vehicles   VehicleNamer
	cfg        Config
	logger     *slog.Logger
	now        func() time.Time
	// nudges coalesces the re-mask sweeps every transition asks for. See
	// nudgeRevalidation.
	nudges *revalidationNudge
}

// NewService builds the shared half.
//
// pusher, activities, revalidate and vehicles may all be nil, and each nil is
// the ordinary unwired state rather than an error: a deployment with no APNs
// key has no pusher, a test that only cares about the window transitions wires
// neither notifier, and every call site below guards. Nothing about a trip's
// STATE depends on any of them — a window opens and closes in the database
// whether or not a single phone hears about it.
func NewService(
	trips TripStore,
	legs LegStore,
	cfg Config,
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	svc := &Service{
		trips:  trips,
		legs:   legs,
		cfg:    cfg.withDefaults(),
		logger: logger,
		now:    time.Now,
	}
	svc.nudges = newRevalidationNudge(svc)
	return svc
}

// WithPushes wires the banner notifier. Optional, following the
// WithRequesterNames precedent in internal/push.
func (s *Service) WithPushes(p Pusher) *Service { s.pusher = p; return s }

// WithActivities wires the Live Activity notifier.
func (s *Service) WithActivities(a ActivityPusher) *Service { s.activities = a; return s }

// WithRevalidator wires the WebSocket access re-mask nudge.
func (s *Service) WithRevalidator(r Revalidator) *Service { s.revalidate = r; return s }

// WithVehicleNames wires the nickname resolver used by the leg card.
func (s *Service) WithVehicleNames(v VehicleNamer) *Service { s.vehicles = v; return s }

// notify sends one trips banner, if a pusher is wired.
func (s *Service) notify(ctx context.Context, p push.TripPush) {
	if s.pusher == nil || len(p.UserIDs) == 0 {
		return
	}
	s.pusher.NotifyTrip(ctx, p)
}

// nudgeRevalidation asks the WebSocket layer to re-derive access and roles now.
//
// COALESCED AND DETACHED, and both halves were bugs before they were choices.
//
// SweepOnce is GLOBAL: it walks every connected session on the hub, whatever
// trip prompted it. Called synchronously once per trip edge, a sweeper pass
// that claimed 200 endings and 200 starts ran FOUR HUNDRED fleet-wide sweeps
// inside one tick — the shape an outage produces, when a backlog of elapsed
// windows is claimed at once — and each of those walks every socket resolving
// a role per (session, vehicle). The same call also sat on the owner's HTTP
// handler: "End trip" returned only after the whole hub had been re-derived.
//
// So a nudge is now a REQUEST rather than a call. One sweep runs at a time; a
// request arriving while one is in flight sets a trailing flag and returns
// immediately, so N edges in a tick cost at most two sweeps — the one already
// running, and one more afterwards that is guaranteed to observe every edge
// that asked. Nothing is lost by coalescing because the sweep takes no
// argument: two sweeps for two trips do exactly what one sweep does.
//
// The context is DETACHED (context.WithoutCancel) and re-bounded, because the
// caller is frequently an HTTP handler whose request context dies the moment
// it answers — the re-mask must outlive the response that triggered it.
func (s *Service) nudgeRevalidation(ctx context.Context, reason, tripID string) {
	if s.revalidate == nil {
		return
	}
	s.nudges.request(context.WithoutCancel(ctx), reason, tripID)
}

// DrainRevalidation waits for any in-flight or trailing re-mask sweep to
// finish. The composition root calls it on shutdown so a detached sweep is not
// abandoned mid-pass, and the package's own tests call it to make an
// asynchronous nudge observable without sleeping.
func (s *Service) DrainRevalidation() {
	if s == nil {
		return
	}
	s.nudges.drain()
}

// vehicleName resolves a nickname for the card, best-effort. "" routes the copy
// to its neutral fallback.
func (s *Service) vehicleName(ctx context.Context, vehicleID string) string {
	if s.vehicles == nil || vehicleID == "" {
		return ""
	}
	name, err := s.vehicles.VehicleName(ctx, vehicleID)
	if err != nil {
		s.logger.Debug("trips: vehicle name lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()))
		return ""
	}
	return name
}

// tripName resolves the trip's name for the card, best-effort. P1 — the value
// reaches the content-state and nothing else, and is never logged.
func (s *Service) tripName(ctx context.Context, tripID string) string {
	name, err := s.trips.TripNameFor(ctx, tripID)
	if err != nil {
		s.logger.Warn("trips: trip name lookup failed; card will carry no trip name",
			slog.String("trip_id", tripID),
			slog.String("error", err.Error()))
		return ""
	}
	return name
}

// discardWriter drops log output for the nil-logger default.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
