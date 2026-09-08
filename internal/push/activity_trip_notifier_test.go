package push

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeTripActivityStore is the in-memory TripActivityStore double.
type fakeTripActivityStore struct {
	mu          sync.Mutex
	tokens      []ActivityStartToken
	activities  []Activity
	endedLegs   []string
	droppedPTS  []string
	droppedAct  []string
	tokensErr   error
	activityErr error
	// marked records every (leg, user) a pass stamped as delivered — the
	// anti-reaper mark. Recorded in call order so a test can assert that the
	// refused sends were NOT stamped.
	marked []string

	// claims models go_trip_activity_tokens.started_leg_id (MYR-612): the leg
	// whose push-to-start was last sent to this device, keyed "trip/user".
	claims     map[string]string
	claimErr   error
	released   []string
	claimCalls int
}

// ClaimPushToStartForLegAll models the one-statement fan-out: it claims every
// registration this leg has not been sent to and returns exactly those. Written
// on top of the same `claims` map the per-device claim uses, so the two cannot
// disagree about who has already been sent.
func (f *fakeTripActivityStore) ClaimPushToStartForLegAll(
	_ context.Context, tripID, legID string,
) ([]ActivityStartToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokensErr != nil {
		return nil, f.tokensErr
	}
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if f.claims == nil {
		f.claims = map[string]string{}
	}
	var out []ActivityStartToken
	for _, tok := range f.tokens {
		f.claimCalls++
		key := tripID + "/" + tok.UserID
		if f.claims[key] == legID {
			continue
		}
		f.claims[key] = legID
		out = append(out, tok)
	}
	return out, nil
}

func (f *fakeTripActivityStore) ClaimPushToStartForLeg(
	_ context.Context, tripID, userID, legID string,
) (ActivityStartToken, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCalls++
	if f.claimErr != nil {
		return ActivityStartToken{}, false, f.claimErr
	}
	if f.claims == nil {
		f.claims = map[string]string{}
	}
	key := tripID + "/" + userID
	if f.claims[key] == legID {
		return ActivityStartToken{}, false, nil
	}
	for _, tok := range f.tokens {
		if tok.UserID != userID {
			continue
		}
		f.claims[key] = legID
		return tok, true, nil
	}
	// No registration for this person: nothing to claim, and not an error.
	return ActivityStartToken{}, false, nil
}

func (f *fakeTripActivityStore) ReleasePushToStartClaim(_ context.Context, tripID, userID, legID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := tripID + "/" + userID
	if f.claims[key] == legID {
		delete(f.claims, key)
	}
	f.released = append(f.released, key+"/"+legID)
	return nil
}

func (f *fakeTripActivityStore) DeleteRejectedPushToStartToken(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.droppedPTS = append(f.droppedPTS, token)
	return nil
}

func (f *fakeTripActivityStore) ActivitiesForLeg(_ context.Context, _ string) ([]Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activityErr != nil {
		return nil, f.activityErr
	}
	return append([]Activity(nil), f.activities...), nil
}

func (f *fakeTripActivityStore) EndActivitiesForLeg(_ context.Context, legID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endedLegs = append(f.endedLegs, legID)
	return int64(len(f.activities)), nil
}

func (f *fakeTripActivityStore) MarkLegActivitiesPushed(
	_ context.Context, legID string, userIDs []string,
) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range userIDs {
		f.marked = append(f.marked, legID+"/"+u)
	}
	return int64(len(userIDs)), nil
}

func (f *fakeTripActivityStore) DeleteActivityToken(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.droppedAct = append(f.droppedAct, token)
	return nil
}

func newTripActivityNotifier(t *testing.T, store *fakeTripActivityStore, prefs Prefs) (*TripActivityNotifier, *FakeActivitySender) {
	t.Helper()
	sender := NewFakeActivitySender()
	prefStore := newFakePrefStore()
	prefStore.byUser["user-a"] = prefs
	prefStore.byUser["user-b"] = prefs
	n := NewTripActivityNotifier(sender, store, prefStore, Config{Enabled: true}, nil)
	n.now = func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }
	return n, sender
}

// TestEndLeg_AlertsOnUpdateThenEnds is the MYR-418 rule, applied verbatim to the
// leg card. An `end` carrying an alert is accepted by APNs and expands nothing,
// so the announcement must ride an UPDATE sent immediately before it — and the
// two pushes must carry DIFFERENT `aps.timestamp` seconds, because ActivityKit
// discards an update that is not strictly newer than what it is showing.
func TestEndLeg_AlertsOnUpdateThenEnds(t *testing.T) {
	store := &fakeTripActivityStore{
		activities: []Activity{{UserID: "user-a", Token: "act-a"}},
	}
	n, sender := newTripActivityNotifier(t, store, DefaultPrefs())

	tc := tripLegFixture()
	tc.Status = tripStatusArrived
	n.EndLeg(context.Background(), tc)

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sent %d activity pushes, want 2 (an alerting update, then the end)", len(sent))
	}
	update, end := sent[0], sent[1]

	if update.Event != ActivityEventUpdate || update.Alert == nil {
		t.Errorf("first push = %q alert=%v, want an ALERTING update — the end expands "+
			"nothing (MYR-418), so this is the sole announcement", update.Event, update.Alert)
	}
	if end.Event != ActivityEventEnd {
		t.Errorf("second push = %q, want end", end.Event)
	}
	if end.Alert != nil {
		t.Error("the end carries an alert; buildActivityPayload drops it, so it would be " +
			"a silent no-op that looks like a working announcement")
	}
	if !end.Timestamp.After(update.Timestamp) {
		t.Errorf("end timestamp %s is not after the update's %s — aps.timestamp renders "+
			"in whole SECONDS, so an equal pair leaves the ordering undefined and "+
			"ActivityKit may discard the end", end.Timestamp, update.Timestamp)
	}
	if end.DismissalDate == nil {
		t.Error("the end has no dismissal-date; the arrival state is the thing worth a look")
	}
	if len(store.endedLegs) != 1 {
		t.Errorf("tombstoned %d legs, want 1 — and AFTER the end push, so a failed end "+
			"leaves a live row the next pass can retry against", len(store.endedLegs))
	}
}

// TestEndLeg_AlertForksOnArrivalEvidence: `completed` is a leg that stopped
// short, and announcing it as an arrival is a small lie on the one surface that
// cannot be corrected afterwards.
func TestEndLeg_AlertForksOnArrivalEvidence(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		wantTitle string
	}{
		{"arrival evidence", tripStatusArrived, "Optimus has arrived"},
		{"parked short", tripStatusCompleted, "Optimus has parked"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeTripActivityStore{
				activities: []Activity{{UserID: "user-a", Token: "act-a"}},
			}
			n, sender := newTripActivityNotifier(t, store, DefaultPrefs())

			tc := tripLegFixture()
			tc.Status = tt.status
			n.EndLeg(context.Background(), tc)

			sent := sender.Sent()
			if len(sent) == 0 || sent[0].Alert == nil {
				t.Fatalf("no alerting update was sent (%d pushes)", len(sent))
			}
			if sent[0].Alert.Title != tt.wantTitle {
				t.Errorf("alert title = %q, want %q", sent[0].Alert.Title, tt.wantTitle)
			}
		})
	}
}

// TestStartLeg_GatesOnTheTripsSwitch pins the difference from the ride path,
// whose twin hardcodes CategoryRideLifecycle: a person who muted RIDES must
// still get their trip cards.
func TestStartLeg_GatesOnTheTripsSwitch(t *testing.T) {
	tests := []struct {
		name        string
		prefs       Prefs
		wantStarted int
	}{
		{"all on", DefaultPrefs(), 1},
		{"trips off", func() Prefs { p := DefaultPrefs(); p.Trips = false; return p }(), 0},
		{"rides off does not silence a trip card",
			func() Prefs { p := DefaultPrefs(); p.RideLifecycle = false; return p }(), 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeTripActivityStore{
				tokens: []ActivityStartToken{{UserID: "user-a", Token: "pts-a"}},
			}
			n, _ := newTripActivityNotifier(t, store, tt.prefs)
			if got := n.StartLeg(context.Background(), tripLegFixture()); got != tt.wantStarted {
				t.Errorf("started %d cards, want %d", got, tt.wantStarted)
			}
		})
	}
}

// TestStartLeg_RejectedTokenGoesFromTheRightTable is the distinction the whole
// push-to-start design turns on: a 410 here means THE APP is gone, and the row
// lives in go_trip_activity_tokens — not in go_live_activities, where the ride
// path's dropActivity would have deleted nothing at all while leaving a dead
// token retried on every remaining leg.
func TestStartLeg_RejectedTokenGoesFromTheRightTable(t *testing.T) {
	store := &fakeTripActivityStore{
		tokens: []ActivityStartToken{{UserID: "user-a", Token: "pts-dead"}},
	}
	n, sender := newTripActivityNotifier(t, store, DefaultPrefs())
	sender.Err = ErrUnregistered

	if got := n.StartLeg(context.Background(), tripLegFixture()); got != 0 {
		t.Errorf("started %d cards on a rejected token, want 0", got)
	}
	if len(store.droppedPTS) != 1 || store.droppedPTS[0] != "pts-dead" {
		t.Errorf("push-to-start drops = %v, want [pts-dead]", store.droppedPTS)
	}
	if len(store.droppedAct) != 0 {
		t.Errorf("the ride table was touched (%v) — a push-to-start 410 must not reach "+
			"go_live_activities", store.droppedAct)
	}
}

// TestStartLeg_NoTokensIsNotAFailure. A leg whose participants are all on the
// web still gets its pushes through the ordinary notifier; the absent card is
// not an error and must not stop anything.
func TestStartLeg_NoTokensIsNotAFailure(t *testing.T) {
	n, sender := newTripActivityNotifier(t, &fakeTripActivityStore{}, DefaultPrefs())
	if got := n.StartLeg(context.Background(), tripLegFixture()); got != 0 {
		t.Errorf("started %d cards with no registrations, want 0", got)
	}
	if len(sender.Sent()) != 0 {
		t.Error("something was sent with no registered tokens")
	}
}

// TestFanOutLeg_StampsOnlyWhatAppleAccepted is finding 7's send-side half.
//
// go_live_activities is reaped 24 hours after each row's last WRITE, and
// `updated_at` only means "last touched" if every delivered pass stamps it. The
// leg path stamped nothing, so a card on a long drive had its registration
// hard-deleted while it was still being pushed to — and the end push then had
// no address at all.
//
// A REFUSED SEND IS NOT A TOUCH. Stamping one would keep a permanently failing
// row alive past the reaper for nobody, which is the ride ticker's rule and the
// reason the mark is built from what Apple accepted rather than from what was
// attempted.
func TestFanOutLeg_StampsOnlyWhatAppleAccepted(t *testing.T) {
	store := &fakeTripActivityStore{
		activities: []Activity{
			{UserID: "user-a", Token: "act-a", TripLegID: "leg-1"},
			{UserID: "user-b", Token: "act-b", TripLegID: "leg-1"},
		},
	}
	n, sender := newTripActivityNotifier(t, store, Prefs{Trips: true})
	// user-b's card is gone from the phone; its token is dropped, not stamped.
	sender.ErrByToken = map[string]error{"act-b": ErrUnregistered}

	n.UpdateLeg(context.Background(), tripLegFixture())

	if len(store.marked) != 1 || store.marked[0] != "leg-1/user-a" {
		t.Errorf("stamped %v, want exactly the accepted delivery leg-1/user-a — a card "+
			"Apple refused is not 'recently pushed'", store.marked)
	}
	if len(store.droppedAct) != 1 || store.droppedAct[0] != "act-b" {
		t.Errorf("dropped %v, want the unregistered UPDATE token act-b", store.droppedAct)
	}
}
