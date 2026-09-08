package telemetry

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// MYR-618 handler tests: a live participant may add people, and may do nothing
// else.
//
// The store is the same hand-written fake the rest of §7.30's handler tests use.
// What is asserted here is the HANDLER's contract — which caller reaches which
// store method, which refusal carries which code, and above all that a REFUSED
// participant PATCH reaches no store method at all. The store's own rules (the
// share join, the ended-window guard, the audit row) are asserted against a real
// Postgres in internal/store.

// newTripRosterHandler wires the two MYR-618 routes for ONE caller.
//
// A local constructor rather than a parameter on the shared newTripTestHandler,
// because these are the first §7.30 tests whose caller is NOT the owner and the
// shared helper hard-codes one. Registering the routes on a mux is required
// rather than tidy: r.PathValue only resolves through a registered pattern, so a
// direct ServeHTTP call would silently see an empty {tripId}.
func newTripRosterHandler(t *testing.T, store TripStore, callerID string, opts ...TripOption) http.Handler {
	t.Helper()

	h := NewTripHandler(
		&stubTokenValidator{userID: callerID},
		store,
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(tripTestOwner)},
		true,
		discardLogger(),
		opts...,
	)
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/trips/{tripId}", h.ServePatch)
	mux.HandleFunc("GET /api/trips/{tripId}/addable-people", h.ServeAddablePeople)
	return mux
}

// participantViewTrip is the fixture trip as the PARTICIPANT sees it: same trip,
// `role: participant`.
func participantViewTrip() TripData {
	trip := fixtureTrip()
	trip.Role = "participant"
	return trip
}

// participantViewTripAfterAdd is the roster one person wider — the state the
// store returns once the add has committed.
func participantViewTripAfterAdd() TripData {
	trip := participantViewTrip()
	adder := "Nabil"
	trip.Participants = append(trip.Participants, TripParticipantData{
		ParticipantID: "csh_2",
		Name:          "Joey",
		UserID:        "usr_joey",
		AddedByName:   &adder,
	})
	return trip
}

// TestParticipantMayAddPeople is the headline: a live participant sending only
// `addParticipantIds` reaches the ADD path, not the owner's patch.
func TestParticipantMayAddPeople(t *testing.T) {
	after := participantViewTripAfterAdd()
	store := &fakeTripStore{trip: participantViewTrip(), addedTrip: &after}
	notifier := &recordingTripNotifier{}
	handler := newTripRosterHandler(t, store, tripTestParticipant, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodPatch, "/api/trips/"+tripTestID,
		`{"addParticipantIds":["csh_2"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if store.addCalls != 1 {
		t.Fatalf("AddTripParticipants called %d times, want 1", store.addCalls)
	}
	// THE ACTOR IS THE CALLER, not the owner. If this ever reads the owner the
	// audit row and the attribution column both start lying.
	if store.lastAddActor != tripTestParticipant {
		t.Errorf("actor = %q, want the calling participant %q", store.lastAddActor, tripTestParticipant)
	}
	if len(store.lastAddShareIDs) != 1 || store.lastAddShareIDs[0] != "csh_2" {
		t.Errorf("share ids = %v, want [csh_2]", store.lastAddShareIDs)
	}
	// The owner's patch method must NOT have been reached — that is the one
	// that can also rename, re-window and remove.
	if store.lastUpdate.Name != nil || store.lastUpdate.RemoveParticipantIDs != nil {
		t.Errorf("the owner's UpdateTrip path was reached: %+v", store.lastUpdate)
	}
}

// TestParticipantAddNotifiesBothSides: the added person hears `trip_added`, and
// the OWNER hears that somebody else widened their roster.
func TestParticipantAddNotifiesBothSides(t *testing.T) {
	after := participantViewTripAfterAdd()
	store := &fakeTripStore{trip: participantViewTrip(), addedTrip: &after}
	notifier := &recordingTripNotifier{}
	handler := newTripRosterHandler(t, store, tripTestParticipant, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodPatch, "/api/trips/"+tripTestID,
		`{"addParticipantIds":["csh_2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if len(notifier.added) != 1 || len(notifier.added[0]) != 1 || notifier.added[0][0] != "usr_joey" {
		t.Fatalf("trip_added audience = %v, want [[usr_joey]] — only the NEW arrival", notifier.added)
	}
	if len(notifier.participantAdded) != 1 {
		t.Fatalf("trip_participant_added sent %d times, want 1", len(notifier.participantAdded))
	}
	got := notifier.participantAdded[0]
	// The names come off the roster we read back, so the banner and the trip
	// sheet cannot call the same person two different things.
	if got.actor != "Nabil" {
		t.Errorf("actor name = %q, want the caller's roster name Nabil", got.actor)
	}
	if len(got.added) != 1 || got.added[0] != "Joey" {
		t.Errorf("added names = %v, want [Joey]", got.added)
	}
}

// TestParticipantAddOfSomebodyAlreadyOnTheTripIsASilent200. The store treats it
// as a no-op; the handler must not manufacture a push out of an unchanged
// roster.
func TestParticipantAddOfSomebodyAlreadyOnTheTripIsASilent200(t *testing.T) {
	unchanged := participantViewTrip()
	store := &fakeTripStore{trip: participantViewTrip(), addedTrip: &unchanged}
	notifier := &recordingTripNotifier{}
	handler := newTripRosterHandler(t, store, tripTestParticipant, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodPatch, "/api/trips/"+tripTestID,
		`{"addParticipantIds":["csh_1"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(notifier.added) != 0 || len(notifier.participantAdded) != 0 {
		t.Errorf("a no-op add announced something: added=%v participantAdded=%v",
			notifier.added, notifier.participantAdded)
	}
}

// TestParticipantMayNotUseOwnerOnlyFields is the refusal, field by field.
//
// ⚠ EVERY CASE ALSO ASSERTS THAT NOTHING REACHED THE STORE. "The whole request
// is refused, nothing applied" is the contract, and a 403 written after a
// partial apply would satisfy the status assertion and violate the rule.
func TestParticipantMayNotUseOwnerOnlyFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"rename", `{"name":"Nabil's trip"}`},
		{"window", `{"endsAt":"2099-01-01T00:00:00Z"}`},
		{"remove", `{"removeParticipantIds":["csh_1"]}`},
		// A PRESENT-BUT-EMPTY owner-only list is still an owner's verb being
		// asked for. Refusing only the requests that would have DONE something
		// would be a rule nobody could state.
		//
		// An explicit JSON `null` is the ONE spelling that is not refused, and
		// that is the contract's own doing rather than a gap: §7.30.4 defines an
		// absent key as UNCHANGED, the owner's path treats `{"name": null}`
		// identically to omitting it, and the schema does not permit null on any
		// of these fields anyway. Refusing it here would make the participant
		// branch stricter than the owner branch about a value that means nothing
		// on either.
		{"empty remove list", `{"removeParticipantIds":[]}`},
		// The add is legal on its own and is refused anyway, because it arrived
		// beside something that is not. A partial apply is the worst of the
		// three available answers.
		{"add beside a removal", `{"addParticipantIds":["csh_2"],"removeParticipantIds":["csh_1"]}`},
		// A malformed instant must be reported as the PERMISSION problem it is,
		// not as a parse error — answering 400 would let a participant probe
		// which fields this server validates and in what order.
		{"malformed window", `{"endsAt":"tomorrow"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeTripStore{trip: participantViewTrip()}
			handler := newTripRosterHandler(t, store, tripTestParticipant)

			rec := tripRequest(t, handler, http.MethodPatch, "/api/trips/"+tripTestID, tc.body)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
			}
			if got := decodeTripError(t, rec).Code; got != wserrors.ErrCodePermissionDenied {
				t.Errorf("code = %q, want permission_denied", got)
			}
			if store.addCalls != 0 {
				t.Errorf("a refused request still called AddTripParticipants %d times", store.addCalls)
			}
		})
	}
}

// TestOwnerPatchIsUnchangedByMYR618. The owner keeps the whole patch, keeps the
// UpdateTrip path, and does NOT receive a banner about their own tap.
func TestOwnerPatchIsUnchangedByMYR618(t *testing.T) {
	store := &fakeTripStore{trip: fixtureTrip()}
	notifier := &recordingTripNotifier{}
	handler := newTripRosterHandler(t, store, tripTestOwner, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodPatch, "/api/trips/"+tripTestID,
		`{"name":"DFW → LA","removeParticipantIds":["csh_1"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if store.addCalls != 0 {
		t.Errorf("the owner was routed through the participant add path")
	}
	if store.lastUpdate.Name == nil || *store.lastUpdate.Name != "DFW → LA" {
		t.Errorf("owner's rename did not reach the store: %+v", store.lastUpdate)
	}
	if len(store.lastUpdate.RemoveParticipantIDs) != 1 {
		t.Errorf("owner's removal did not reach the store: %+v", store.lastUpdate)
	}
	if len(notifier.participantAdded) != 0 {
		t.Errorf("the owner was told about their own roster change: %v", notifier.participantAdded)
	}
}

// TestParticipantAddRefusalsRideTheExistingErrorMap. The store's sentinels are
// mapped exactly as they are on the owner's add — a refusal must not depend on
// who asked, or the status becomes an oracle for the caller's role.
func TestParticipantAddRefusalsRideTheExistingErrorMap(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		status  int
		code    wserrors.ErrorCode
		subCode wserrors.SubCode
	}{
		{
			// A SUSPENDED grant is one member of the deliberately-unspecific
			// class `participant_not_shared` covers — beside "no such share",
			// "a share on a different car" and "an invite never redeemed".
			// Naming which would make the endpoint an oracle for other people's
			// share ids.
			name: "suspended or unshared target", err: ErrTripParticipantNotShared,
			status: http.StatusBadRequest, code: wserrors.ErrCodeInvalidRequest,
			subCode: SubCodeParticipantNotShared,
		},
		{
			name: "the trip has ended", err: ErrTripEnded,
			status: http.StatusConflict, code: wserrors.ErrCodeConflict,
			subCode: SubCodeTripEnded,
		},
		{
			// REVIEW ROUND. The owner removed this person, and only the owner
			// may add them back. A `conflict` rather than a
			// `permission_denied` — the caller HOLDS the verb, and it is this
			// particular person who may not be added — and not
			// `participant_not_shared`, whose advice ("get a share first")
			// leads nowhere since they very likely still hold one.
			name: "the owner removed them", err: ErrTripParticipantOwnerRemoved,
			status: http.StatusConflict, code: wserrors.ErrCodeConflict,
			subCode: SubCodeParticipantOwnerRemoved,
		},
		{
			name: "not on the trip at all", err: ErrTripNotFound,
			status: http.StatusNotFound, code: wserrors.ErrCodeNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeTripStore{trip: participantViewTrip(), addErr: tc.err}
			handler := newTripRosterHandler(t, store, tripTestParticipant)

			rec := tripRequest(t, handler, http.MethodPatch, "/api/trips/"+tripTestID,
				`{"addParticipantIds":["csh_9"]}`)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			env := decodeTripError(t, rec)
			if env.Code != tc.code {
				t.Errorf("code = %q, want %q", env.Code, tc.code)
			}
			if tc.subCode != "" {
				if env.SubCode == nil || wserrors.SubCode(*env.SubCode) != tc.subCode {
					t.Errorf("subCode = %v, want %q", env.SubCode, tc.subCode)
				}
			}
		})
	}
}

// TestAddablePeopleShape pins the wire shape a picker decodes.
func TestAddablePeopleShape(t *testing.T) {
	store := &fakeTripStore{
		trip: participantViewTrip(),
		addable: []TripAddablePersonData{
			{ShareID: "csh_2", Name: "Joey"},
			{ShareID: "csh_3", Name: "Mom"},
		},
	}
	handler := newTripRosterHandler(t, store, tripTestParticipant)

	rec := tripRequest(t, handler, http.MethodGet, "/api/trips/"+tripTestID+"/addable-people", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		People []map[string]any `json:"people"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.People) != 2 {
		t.Fatalf("people = %d, want 2", len(body.People))
	}
	if body.People[0]["shareId"] != "csh_2" || body.People[0]["displayName"] != "Joey" {
		t.Errorf("first row = %+v, want {shareId: csh_2, displayName: Joey}", body.People[0])
	}
	// NAMES ONLY. §7.5's grant listing stays owner-only; a picker reached
	// through a trip must not become a way for a participant to read invite
	// codes, email addresses or per-grant permissions.
	for _, row := range body.People {
		if len(row) != 2 {
			t.Errorf("row carries %d keys, want exactly shareId and displayName: %+v", len(row), row)
		}
	}
}

// TestAddablePeopleIsEmptyRatherThanAbsentWhenEverybodyIsAboard. The envelope is
// always `{people: [...]}`; an empty list is a list, not a null.
func TestAddablePeopleIsEmptyRatherThanAbsentWhenEverybodyIsAboard(t *testing.T) {
	store := &fakeTripStore{trip: participantViewTrip()}
	handler := newTripRosterHandler(t, store, tripTestParticipant)

	rec := tripRequest(t, handler, http.MethodGet, "/api/trips/"+tripTestID+"/addable-people", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "{\"people\":[]}\n" {
		t.Errorf("body = %q, want an empty ARRAY rather than null", got)
	}
}

// TestAddablePeopleRefusesAStrangerAs404. The 404-not-403 rule applies to the
// new route exactly as it does to every other per-trip read: a trip the caller
// is not on must be indistinguishable from a trip that does not exist.
func TestAddablePeopleRefusesAStrangerAs404(t *testing.T) {
	store := &fakeTripStore{addableErr: ErrTripNotFound}
	handler := newTripRosterHandler(t, store, "usr_stranger")

	rec := tripRequest(t, handler, http.MethodGet, "/api/trips/"+tripTestID+"/addable-people", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if got := decodeTripError(t, rec).Code; got != wserrors.ErrCodeNotFound {
		t.Errorf("code = %q, want not_found — a 403 would confirm the trip is real", got)
	}
}

// TestAddablePeopleRefusesAnEndedTripAs409 is review finding 4 at the wire.
//
// The picker refuses on a closed window with the SAME `409 conflict /
// trip_ended` §7.30.4's add gives, so a client that already handles the add's
// refusal handles this one, and it never lists names the very next request
// would reject.
func TestAddablePeopleRefusesAnEndedTripAs409(t *testing.T) {
	store := &fakeTripStore{addableErr: ErrTripEnded}
	handler := newTripRosterHandler(t, store, tripTestParticipant)

	rec := tripRequest(t, handler, http.MethodGet, "/api/trips/"+tripTestID+"/addable-people", "")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	env := decodeTripError(t, rec)
	if env.Code != wserrors.ErrCodeConflict {
		t.Errorf("code = %q, want conflict", env.Code)
	}
	if env.SubCode == nil || wserrors.SubCode(*env.SubCode) != SubCodeTripEnded {
		t.Errorf("subCode = %v, want %q — a bare conflict reads as a server bug and the "+
			"client retries", env.SubCode, SubCodeTripEnded)
	}
}

// TestRosterCarriesAddedByName. The attribution is on every roster row, always
// present, and null rather than absent when nobody recorded an adder.
func TestRosterCarriesAddedByName(t *testing.T) {
	after := participantViewTripAfterAdd()
	store := &fakeTripStore{trip: participantViewTrip(), addedTrip: &after}
	handler := newTripRosterHandler(t, store, tripTestParticipant)

	rec := tripRequest(t, handler, http.MethodPatch, "/api/trips/"+tripTestID,
		`{"addParticipantIds":["csh_2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Participants []map[string]any `json:"participants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Participants) != 2 {
		t.Fatalf("participants = %d, want 2", len(body.Participants))
	}
	for _, row := range body.Participants {
		if _, present := row["addedByName"]; !present {
			t.Errorf("addedByName is ABSENT on %+v — the key must always be there, null when unknown", row)
		}
	}
	if body.Participants[0]["addedByName"] != nil {
		t.Errorf("a row with no recorded adder rendered %v, want null", body.Participants[0]["addedByName"])
	}
	if body.Participants[1]["addedByName"] != "Nabil" {
		t.Errorf("addedByName = %v, want Nabil", body.Participants[1]["addedByName"])
	}
}
