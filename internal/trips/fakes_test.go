package trips

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/push"
)

// The doubles. Deliberately hand-written rather than generated: every one of
// them models a CLAIM, and a claim that a fake grants twice is a test that
// passes while the real thing double-notifies.

type fakeTripStore struct {
	mu sync.Mutex

	audience   map[string]TripAudience
	names      map[string]string
	toStart    []string
	toEnd      []string
	started    map[string]bool
	ended      map[string]bool
	vehicles   []TripVehicle
	claimErr   error
	audErr     error
	vehiclErr  error
	confirmErr error
	// vehicleCalls counts candidate reads, so a test can assert the frame path
	// is not querying per frame.
	vehicleCalls int
	// audienceCalls counts audience reads. MYR-612: this read used to happen on
	// EVERY frame of a car with an open leg; it now happens only at an edge.
	audienceCalls int
}

func newFakeTripStore() *fakeTripStore {
	return &fakeTripStore{
		audience: map[string]TripAudience{},
		names:    map[string]string{},
		started:  map[string]bool{},
		ended:    map[string]bool{},
	}
}

func (f *fakeTripStore) TripAudienceFor(_ context.Context, tripID string) (TripAudience, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audienceCalls++
	if f.audErr != nil {
		return TripAudience{}, f.audErr
	}
	a, ok := f.audience[tripID]
	if !ok {
		return TripAudience{}, errors.New("no such trip")
	}
	return a, nil
}

func (f *fakeTripStore) TripNameFor(_ context.Context, tripID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.names[tripID], nil
}

// ClaimTripsToStart hands back the queued ids ONCE, exactly as the real
// UPDATE … RETURNING does: a second pass over the same trips returns nothing.
func (f *fakeTripStore) ClaimTripsToStart(_ context.Context, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	var out []string
	for _, id := range f.toStart {
		if f.started[id] {
			continue
		}
		f.started[id] = true
		out = append(out, id)
	}
	return out, nil
}

func (f *fakeTripStore) ClaimTripsToEnd(_ context.Context, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	var out []string
	for _, id := range f.toEnd {
		if f.ended[id] {
			continue
		}
		f.ended[id] = true
		out = append(out, id)
	}
	return out, nil
}

func (f *fakeTripStore) ClaimTripStartNow(_ context.Context, tripID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started[tripID] {
		return false, nil
	}
	f.started[tripID] = true
	return true, nil
}

func (f *fakeTripStore) ClaimTripEndNow(_ context.Context, tripID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ended[tripID] {
		return false, nil
	}
	f.ended[tripID] = true
	return true, nil
}

// ActiveTripForVehicle models the confirmation openLeg runs before it writes.
// It answers from the SAME `vehicles` slice the candidate snapshot is built
// from, so a test that removes a car from the snapshot has also closed its
// window — which is the real relationship between the two statements.
func (f *fakeTripStore) ActiveTripForVehicle(_ context.Context, vehicleID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.confirmErr != nil {
		return "", f.confirmErr
	}
	for _, tv := range f.vehicles {
		if tv.VehicleID == vehicleID {
			return tv.TripID, nil
		}
	}
	return "", nil
}

func (f *fakeTripStore) ActiveTripVehicles(_ context.Context, _ int) ([]TripVehicle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vehicleCalls++
	if f.vehiclErr != nil {
		return nil, f.vehiclErr
	}
	return append([]TripVehicle(nil), f.vehicles...), nil
}

type fakeLegStore struct {
	mu sync.Mutex

	byID      map[string]*Leg
	openByVeh map[string]string
	nextID    int
	claimed   map[string]bool
	// arrived models go_trip_legs.arrived, which trips.Leg does not carry: the
	// resume probe refuses a leg that ARRIVED, so the fake has to remember it.
	arrived     map[string]bool
	startErr    error
	openErr     error
	resumeErr   error
	startCalls  int
	resumeCalls int
	// openCalls counts OpenLegForVehicle reads. MYR-612: one per frame before
	// the LegReadTTL cache, roughly one per TTL after it.
	openCalls int
}

func newFakeLegStore() *fakeLegStore {
	return &fakeLegStore{
		byID:      map[string]*Leg{},
		openByVeh: map[string]string{},
		claimed:   map[string]bool{},
		arrived:   map[string]bool{},
	}
}

func (f *fakeLegStore) StartLeg(_ context.Context, tripID, vehicleID, destination string, at time.Time) (Leg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if f.startErr != nil {
		return Leg{}, f.startErr
	}
	// The one-open-leg-per-trip index, modelled: an existing open leg is
	// RETURNED rather than replaced, which is what makes a redelivered edge
	// harmless.
	if id, ok := f.openByVeh[vehicleID]; ok {
		return *f.byID[id], nil
	}
	f.nextID++
	leg := &Leg{
		ID: idFor(f.nextID), TripID: tripID, VehicleID: vehicleID,
		DestinationName: destination, StartedAt: at,
	}
	f.byID[leg.ID] = leg
	f.openByVeh[vehicleID] = leg.ID
	return *leg, nil
}

// ResumeRecentLeg models the store's merge: the leg this car closed most
// recently WITHOUT ARRIVING, if it closed since notBefore and was going to the
// same place, is re-opened rather than replaced.
func (f *fakeLegStore) ResumeRecentLeg(
	_ context.Context, vehicleID, destination string, notBefore time.Time,
) (Leg, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	if f.resumeErr != nil {
		return Leg{}, false, f.resumeErr
	}
	if destination == "" {
		return Leg{}, false, nil
	}
	if _, open := f.openByVeh[vehicleID]; open {
		return Leg{}, false, nil
	}
	var best *Leg
	for _, leg := range f.byID {
		if leg.VehicleID != vehicleID || leg.EndedAt == nil || f.arrived[leg.ID] {
			continue
		}
		if leg.EndedAt.Before(notBefore) || leg.DestinationName != destination {
			continue
		}
		if best == nil || leg.EndedAt.After(*best.EndedAt) {
			best = leg
		}
	}
	if best == nil {
		return Leg{}, false, nil
	}
	best.EndedAt = nil
	if f.claimed["act_end:"+best.ID] {
		// A card that was ENDED is gone from the lock screen, so the resumed
		// leg may raise a new one: its push-to-start claim is released. A card
		// still running keeps its claim and is not started twice.
		delete(f.claimed, "act_start:"+best.ID)
	}
	delete(f.claimed, "act_end:"+best.ID)
	f.openByVeh[vehicleID] = best.ID
	return *best, true, nil
}

func (f *fakeLegStore) EndLeg(_ context.Context, legID string, endedAt time.Time, arrived bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	leg, ok := f.byID[legID]
	if !ok || leg.EndedAt != nil {
		return nil
	}
	end := endedAt
	leg.EndedAt = &end
	f.arrived[legID] = f.arrived[legID] || arrived
	delete(f.openByVeh, leg.VehicleID)
	return nil
}

func (f *fakeLegStore) OpenLegForVehicle(_ context.Context, vehicleID string) (Leg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls++
	if f.openErr != nil {
		return Leg{}, f.openErr
	}
	id, ok := f.openByVeh[vehicleID]
	if !ok {
		return Leg{}, nil
	}
	return *f.byID[id], nil
}

func (f *fakeLegStore) OpenLegsForTrip(_ context.Context, tripID string) ([]Leg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Leg
	for _, leg := range f.byID {
		if leg.TripID == tripID && leg.EndedAt == nil {
			out = append(out, *leg)
		}
	}
	return out, nil
}

func (f *fakeLegStore) claim(key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed[key] {
		return false, nil
	}
	f.claimed[key] = true
	return true, nil
}

func (f *fakeLegStore) ClaimLegStartedPush(_ context.Context, legID string) (bool, error) {
	return f.claim("started:" + legID)
}

func (f *fakeLegStore) ClaimLegArrivedPush(_ context.Context, legID string) (bool, error) {
	return f.claim("arrived:" + legID)
}

func (f *fakeLegStore) ClaimLegActivityStart(_ context.Context, legID string) (bool, error) {
	return f.claim("act_start:" + legID)
}

func (f *fakeLegStore) ClaimLegActivityEnd(_ context.Context, legID string) (bool, error) {
	return f.claim("act_end:" + legID)
}

// idFor mints a readable leg id. strconv rather than rune arithmetic so gosec's
// integer-conversion check has nothing to flag and the ids stay legible past
// twenty-six legs.
func idFor(n int) string { return "leg-" + strconv.Itoa(n) }

type fakePusher struct {
	mu   sync.Mutex
	sent []push.TripPush
}

func (f *fakePusher) NotifyTrip(_ context.Context, p push.TripPush) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, p)
}

func (f *fakePusher) events() []push.TripEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]push.TripEvent, 0, len(f.sent))
	for _, p := range f.sent {
		out = append(out, p.Event)
	}
	return out
}

type fakeActivityPusher struct {
	mu      sync.Mutex
	starts  []push.TripLegContext
	updates []push.TripLegContext
	ends    []push.TripLegContext
}

func (f *fakeActivityPusher) StartLeg(_ context.Context, tc push.TripLegContext) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, tc)
	return 1
}

func (f *fakeActivityPusher) UpdateLeg(_ context.Context, tc push.TripLegContext) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, tc)
	return 1
}

func (f *fakeActivityPusher) EndLeg(_ context.Context, tc push.TripLegContext) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, tc)
}

type fakeRevalidator struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeRevalidator) SweepOnce(_ context.Context) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return 0
}

func (f *fakeRevalidator) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeVINResolver struct{ byVIN map[string]string }

func (f *fakeVINResolver) ResolveID(_ context.Context, vin string) (string, error) {
	return f.byVIN[vin], nil
}
