package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Handler tests for the MYR-602 trips surface (§7.30).
//
// The store is a hand-written fake — no database, no testcontainers — because
// what these assert is the HANDLER's contract: which status each refusal
// produces, which sub-code rides it, what reaches the wire, and what does not.
// The store's own rules (the window, the overlap, the share join) are asserted
// against a real Postgres in internal/store.

const (
	tripTestOwner       = "usr_owner"
	tripTestParticipant = "usr_participant"
	tripTestVehicle     = "cveh0000000000000000000001"
	tripTestID          = "ctrp0000000000000000000001"
)

// fakeTripStore records calls and replays scripted answers.
type fakeTripStore struct {
	trip TripData
	err  error

	// leaveCalls counts LeaveTrip invocations so the idempotency assertions can
	// prove the handler still reached the store rather than short-circuiting.
	leaveCalls  int
	tokenCalls  int
	deleteCalls int

	// tripDeleteCalls counts DeleteTrip invocations, and deleteErr is separate
	// from err so a test can script "the read succeeds, the delete fails" —
	// the settle-before-delete ordering's own failure mode.
	tripDeleteCalls int
	deleteErr       error
	// endCalls counts EndTrip, which the delete route calls before settling so
	// a failed delete leaves a trip that is genuinely over.
	endCalls int

	// The LEG anchor of §7.21's per-Activity path (§7.21.7).
	legTokenCalls  int
	legEndCalls    int
	legAccessCalls int
	lastLegTripID  string
	lastLegID      string
	lastLegToken   string
	lastLegSandbox bool
	legRegisterErr error
	// legAccess models store.TripLegAccess: the trip the leg belongs to,
	// whether it is still open, and the refusal for a caller who is not on it.
	legAccessTripID string
	legAccessOpen   bool
	legAccessErr    error
	// legEndErr is separate from err so a test can express the DELETE's whole
	// point: the trip read would refuse this caller, and the delete succeeds
	// anyway because it only ever touches their own row.
	legEndErr error
	legEnded  bool

	// lastCreate captures the input so the path-vs-body vehicleId rule can be
	// asserted directly.
	lastCreate TripCreateInput
	lastUpdate TripUpdateInput

	// MYR-618. addCalls counts AddTripParticipants so a refused participant
	// PATCH can be proven to have reached no store at all, and lastAddShareIDs
	// / lastAddActor capture what it was asked to do.
	addCalls        int
	lastAddShareIDs []string
	lastAddActor    string
	addErr          error
	// addedTrip replaces `trip` as the AddTripParticipants result when set, so
	// a test can script the before/after roster diff the push fan-out is built
	// on.
	addedTrip *TripData

	addable       []TripAddablePersonData
	addableErr    error
	addableCalls  int
	lastAddableID string
}

func (f *fakeTripStore) AddTripParticipants(
	_ context.Context, _, actorUserID string, shareIDs []string,
) (TripData, error) {
	f.addCalls++
	f.lastAddActor = actorUserID
	f.lastAddShareIDs = shareIDs
	if f.addErr != nil {
		return TripData{}, f.addErr
	}
	if f.addedTrip != nil {
		return *f.addedTrip, nil
	}
	return f.trip, f.err
}

func (f *fakeTripStore) TripAddablePeople(_ context.Context, tripID, _ string) ([]TripAddablePersonData, error) {
	f.addableCalls++
	f.lastAddableID = tripID
	if f.addableErr != nil {
		return nil, f.addableErr
	}
	return f.addable, nil
}

func (f *fakeTripStore) CreateTrip(_ context.Context, in TripCreateInput) (TripData, error) {
	f.lastCreate = in
	return f.trip, f.err
}
func (f *fakeTripStore) GetTrip(context.Context, string, string) (TripData, error) {
	return f.trip, f.err
}
func (f *fakeTripStore) ListTrips(context.Context, string, string, int) ([]TripData, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []TripData{f.trip}, nil
}
func (f *fakeTripStore) UpdateTrip(_ context.Context, _, _ string, in TripUpdateInput) (TripData, error) {
	f.lastUpdate = in
	return f.trip, f.err
}
func (f *fakeTripStore) EndTrip(context.Context, string, string) (TripData, error) {
	f.endCalls++
	return f.trip, f.err
}
func (f *fakeTripStore) LeaveTrip(context.Context, string, string) error {
	f.leaveCalls++
	return f.err
}
func (f *fakeTripStore) DeleteTrip(context.Context, string, string) error {
	f.tripDeleteCalls++
	return f.deleteErr
}
func (f *fakeTripStore) TripDrives(context.Context, string, string, DriveListCursor, int) (DriveListPage, error) {
	if f.err != nil {
		return DriveListPage{}, f.err
	}
	return DriveListPage{}, nil
}
func (f *fakeTripStore) RegisterTripActivityStartToken(context.Context, string, string, string, bool) error {
	f.tokenCalls++
	return f.err
}
func (f *fakeTripStore) DeleteTripActivityStartToken(context.Context, string, string) error {
	f.deleteCalls++
	return f.err
}
func (f *fakeTripStore) TripLegAccess(_ context.Context, legID, _ string) (string, bool, error) {
	f.legAccessCalls++
	f.lastLegID = legID
	if f.legAccessErr != nil {
		return "", false, f.legAccessErr
	}
	return f.legAccessTripID, f.legAccessOpen, nil
}
func (f *fakeTripStore) RegisterTripLegActivityToken(
	_ context.Context, tripID, legID, _, token string, sandbox bool,
) error {
	f.legTokenCalls++
	f.lastLegTripID, f.lastLegID, f.lastLegToken, f.lastLegSandbox = tripID, legID, token, sandbox
	if f.legRegisterErr != nil {
		return f.legRegisterErr
	}
	return f.err
}
func (f *fakeTripStore) EndTripLegActivityToken(_ context.Context, legID, _ string) (bool, error) {
	f.legEndCalls++
	f.lastLegID = legID
	return f.legEnded, f.legEndErr
}

// recordingNotifier captures the fan-out so the "who gets told" rules are
// assertable without a push pipeline.
type recordingTripNotifier struct {
	added   [][]string
	started [][]string
	ended   [][]string
	deleted [][]string
	// participantAdded records MYR-618's owner-directed banner.
	participantAdded []participantAdd

	// onDeleted lets a test observe the ORDER of the two calls the delete route
	// makes, which is the one property no status code can express.
	onDeleted func()
}

func (n *recordingTripNotifier) TripAdded(_ context.Context, _ TripData, ids []string) {
	n.added = append(n.added, ids)
}
func (n *recordingTripNotifier) TripStarted(_ context.Context, _ TripData, ids []string) {
	n.started = append(n.started, ids)
}
func (n *recordingTripNotifier) TripEnded(_ context.Context, _ TripData, ids []string) {
	n.ended = append(n.ended, ids)
}

// participantAdd records one MYR-618 owner banner: who added, and whom.
type participantAdd struct {
	actor string
	added []string
}

func (n *recordingTripNotifier) TripParticipantAdded(
	_ context.Context, _ TripData, actorName string, addedNames []string,
) {
	n.participantAdded = append(n.participantAdded, participantAdd{actor: actorName, added: addedNames})
}

func (n *recordingTripNotifier) TripDeleted(_ context.Context, _ TripData, ids []string) {
	n.deleted = append(n.deleted, ids)
	if n.onDeleted != nil {
		n.onDeleted()
	}
}

// fixtureTrip is an ACTIVE trip owned by tripTestOwner with one participant.
func fixtureTrip() TripData {
	now := time.Now()
	return TripData{
		ID:        tripTestID,
		VehicleID: tripTestVehicle,
		Name:      "DFW → LA",
		StartsAt:  now.Add(-2 * time.Hour),
		EndsAt:    now.Add(48 * time.Hour),
		CreatedAt: now.Add(-2 * time.Hour),
		Role:      tripRoleOwner,
		Vehicle: TripVehicleData{
			VehicleID: tripTestVehicle,
			Name:      "Roadie",
			Model:     "Model Y",
			Year:      2024,
			Color:     "UltraRed",
			VIN:       "7SAYGDET7TA613795",
		},
		Participants: []TripParticipantData{
			{ParticipantID: "csh_1", Name: "Nabil", UserID: tripTestParticipant},
		},
		DriveCount: 3,
	}
}

// newTripTestHandler wires the handler onto a mux, which is required rather
// than optional: r.PathValue only resolves through a registered pattern, so a
// direct ServeHTTP call would silently see an empty {tripId}.
func newTripTestHandler(t *testing.T, store TripStore, enabled bool, opts ...TripOption) http.Handler {
	t.Helper()

	h := NewTripHandler(
		&stubTokenValidator{userID: tripTestOwner},
		store,
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(tripTestOwner)},
		enabled,
		discardLogger(),
		opts...,
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/vehicles/{vehicleId}/trips", h.ServeCreate)
	mux.HandleFunc("GET /api/trips", h.ServeList)
	mux.HandleFunc("GET /api/trips/{tripId}", h.ServeGet)
	mux.HandleFunc("PATCH /api/trips/{tripId}", h.ServePatch)
	mux.HandleFunc("DELETE /api/trips/{tripId}", h.ServeDelete)
	mux.HandleFunc("POST /api/trips/{tripId}/end", h.ServeEnd)
	mux.HandleFunc("DELETE /api/trips/{tripId}/participants/me", h.ServeLeave)
	mux.HandleFunc("GET /api/trips/{tripId}/drives", h.ServeDrives)
	mux.HandleFunc("GET /api/trips/{tripId}/addable-people", h.ServeAddablePeople)
	mux.HandleFunc("POST /api/trips/{tripId}/activity-start-token", h.ServeRegisterActivityToken)
	mux.HandleFunc("DELETE /api/trips/{tripId}/activity-start-token", h.ServeDeleteActivityToken)
	mux.HandleFunc("POST /api/trip-legs/{legId}/activity-token", h.ServeRegisterLegActivityToken)
	mux.HandleFunc("DELETE /api/trip-legs/{legId}/activity-token", h.ServeEndLegActivityToken)
	return mux
}

func tripRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeTripError(t *testing.T, rec *httptest.ResponseRecorder) wserrors.ErrorEnvelopeBody {
	t.Helper()

	var env wserrors.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env.Error
}

// TestTripsKillSwitchAnswers503OnEveryRoute pins that TRIPS_ENABLED=false
// switches the feature off WHOLE.
//
// 503 AND NOT 404 is the assertion that matters. The routes exist and will work
// again; a 404 tells a client the feature does not exist, and some clients cache
// that decision. And the READS are off too — leaving GET alive would show an
// owner a live trip card whose every button returns an error.
func TestTripsKillSwitchAnswers503OnEveryRoute(t *testing.T) {
	handler := newTripTestHandler(t, &fakeTripStore{trip: fixtureTrip()}, false)

	routes := []struct{ method, path string }{
		{http.MethodPost, "/api/vehicles/" + tripTestVehicle + "/trips"},
		{http.MethodGet, "/api/trips"},
		{http.MethodGet, "/api/trips/" + tripTestID},
		{http.MethodPatch, "/api/trips/" + tripTestID},
		{http.MethodDelete, "/api/trips/" + tripTestID},
		{http.MethodPost, "/api/trips/" + tripTestID + "/end"},
		{http.MethodDelete, "/api/trips/" + tripTestID + "/participants/me"},
		{http.MethodGet, "/api/trips/" + tripTestID + "/drives"},
		{http.MethodPost, "/api/trips/" + tripTestID + "/activity-start-token"},
		{http.MethodDelete, "/api/trips/" + tripTestID + "/activity-start-token"},
		{http.MethodPost, "/api/trip-legs/cleg_1/activity-token"},
		{http.MethodDelete, "/api/trip-legs/cleg_1/activity-token"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := tripRequest(t, handler, rt.method, rt.path, "{}")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503. Body: %s", rec.Code, rec.Body.String())
			}
			if got := decodeTripError(t, rec).Code; got != wserrors.ErrCodeServiceUnavailable {
				t.Errorf("code = %q, want service_unavailable", got)
			}
		})
	}
}

// TestTripErrorsMapToTheirStatusAndSubCode is the whole error table, asserted
// through the real handler rather than by reading writeTripError.
//
// The SUB-CODES are the point. `conflict` alone cannot tell a client whether to
// send the owner back to the date picker (a double-booked car) or to tell them
// the trip is over and offer a new one, and a message is not something a client
// may branch on (§4.1 rule 1).
func TestTripErrorsMapToTheirStatusAndSubCode(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    wserrors.ErrorCode
		wantSubCode string
	}{
		{
			name: "a trip the caller is not on is 404, never 403",
			err:  ErrTripNotFound, wantStatus: http.StatusNotFound, wantCode: wserrors.ErrCodeNotFound,
		},
		{
			name: "an overlapping window is a sub-coded conflict",
			err:  ErrTripOverlaps, wantStatus: http.StatusConflict, wantCode: wserrors.ErrCodeConflict,
			wantSubCode: string(SubCodeTripOverlaps),
		},
		{
			name: "a mutation on an ended trip is a sub-coded conflict",
			err:  ErrTripEnded, wantStatus: http.StatusConflict, wantCode: wserrors.ErrCodeConflict,
			wantSubCode: string(SubCodeTripEnded),
		},
		{
			name: "an unshared participant is a sub-coded 400",
			err:  ErrTripParticipantNotShared, wantStatus: http.StatusBadRequest, wantCode: wserrors.ErrCodeInvalidRequest,
			wantSubCode: string(SubCodeParticipantNotShared),
		},
		{
			name: "a bad window is a plain 400",
			err:  ErrTripWindowInvalid, wantStatus: http.StatusBadRequest, wantCode: wserrors.ErrCodeInvalidRequest,
		},
		{
			name: "a bad name is a plain 400",
			err:  ErrTripNameInvalid, wantStatus: http.StatusBadRequest, wantCode: wserrors.ErrCodeInvalidRequest,
		},
		{
			// An UNRECOGNISED error is a bug, not a client mistake. Reporting
			// it as a 4xx would tell the caller to change a request when
			// nothing they could do would help.
			name: "an unknown error is 500, not a refusal",
			err:  errors.New("connection reset"), wantStatus: http.StatusInternalServerError,
			wantCode: wserrors.ErrCodeInternalError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTripTestHandler(t, &fakeTripStore{err: tc.err}, true)
			rec := tripRequest(t, handler, http.MethodGet, "/api/trips/"+tripTestID, "")

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d. Body: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := decodeTripError(t, rec)
			if body.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", body.Code, tc.wantCode)
			}
			switch {
			case tc.wantSubCode == "":
				if body.SubCode != nil {
					t.Errorf("subCode = %q, want none", *body.SubCode)
				}
			case body.SubCode == nil:
				t.Errorf("subCode missing, want %q", tc.wantSubCode)
			case *body.SubCode != tc.wantSubCode:
				t.Errorf("subCode = %q, want %q", *body.SubCode, tc.wantSubCode)
			}
		})
	}
}

// TestTripErrorMessagesCarryNoUserContent is the P1 tripwire.
//
// A trip name is user content and an error body is a value's most reliable
// route into a log, a proxy trace and a crash report. No refusal on this
// surface may echo one.
func TestTripErrorMessagesCarryNoUserContent(t *testing.T) {
	const secret = "Nabil's anniversary weekend" //nolint:gosec // G101: a trip NAME fixture, which is the P1 value under test.

	trip := fixtureTrip()
	trip.Name = secret
	handler := newTripTestHandler(t, &fakeTripStore{trip: trip, err: ErrTripOverlaps}, true)

	rec := tripRequest(t, handler, http.MethodPost, "/api/vehicles/"+tripTestVehicle+"/trips",
		`{"name":"`+secret+`","startsAt":"2026-09-06T00:00:00Z","endsAt":"2026-09-08T00:00:00Z","participantIds":[]}`)

	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("the refusal echoed the trip name: %s", rec.Body.String())
	}
}

// TestCreateTakesTheVehicleFromThePath pins the authority rule in both
// directions.
//
// CreateTripRequest carries a REQUIRED `vehicleId`, so the strict decode has to
// accept it — dropping the field from the struct would 400 every conformant
// request. But the PATH is what was ownership-checked and the path is what the
// trip is created on. A body that AGREES is used; a body that disagrees is
// refused rather than silently overridden, because a client that got its own
// body wrong would otherwise create a trip on a different car and find out from
// a support ticket.
func TestCreateTakesTheVehicleFromThePath(t *testing.T) {
	t.Run("an agreeing body is accepted and the path is used", func(t *testing.T) {
		store := &fakeTripStore{trip: fixtureTrip()}
		handler := newTripTestHandler(t, store, true)

		rec := tripRequest(t, handler, http.MethodPost, "/api/vehicles/"+tripTestVehicle+"/trips",
			`{"vehicleId":"`+tripTestVehicle+`","name":"Trip","startsAt":"2026-09-06T00:00:00Z","endsAt":"2026-09-08T00:00:00Z","participantIds":[]}`)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201. Body: %s", rec.Code, rec.Body.String())
		}
		if store.lastCreate.VehicleID != tripTestVehicle {
			t.Fatalf("create used vehicle %q, want the PATH's %q", store.lastCreate.VehicleID, tripTestVehicle)
		}
	})

	t.Run("a disagreeing body is refused, never silently overridden", func(t *testing.T) {
		store := &fakeTripStore{trip: fixtureTrip()}
		handler := newTripTestHandler(t, store, true)

		rec := tripRequest(t, handler, http.MethodPost, "/api/vehicles/"+tripTestVehicle+"/trips",
			`{"vehicleId":"cveh0000000000000000000099","name":"Trip","startsAt":"2026-09-06T00:00:00Z","endsAt":"2026-09-08T00:00:00Z","participantIds":[]}`)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400. Body: %s", rec.Code, rec.Body.String())
		}
		if store.lastCreate.VehicleID != "" {
			t.Fatalf("the store was reached with %q despite the mismatch", store.lastCreate.VehicleID)
		}
	})

	t.Run("an omitted body vehicleId still works", func(t *testing.T) {
		store := &fakeTripStore{trip: fixtureTrip()}
		handler := newTripTestHandler(t, store, true)

		rec := tripRequest(t, handler, http.MethodPost, "/api/vehicles/"+tripTestVehicle+"/trips",
			`{"name":"Trip","startsAt":"2026-09-06T00:00:00Z","endsAt":"2026-09-08T00:00:00Z","participantIds":[]}`)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201. Body: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestTripWireDropsEveryParticipantUserID is the roster's privacy tripwire.
//
// Everyone on a trip sees the whole roster — they are on a trip together — but
// nobody receives anybody's USER ID. Publishing one would hand every
// participant a durable identifier for everybody else, which no part of the
// product asks for. `userIsSelf` is the whole of what the caller needs.
func TestTripWireDropsEveryParticipantUserID(t *testing.T) {
	handler := newTripTestHandler(t, &fakeTripStore{trip: fixtureTrip()}, true)
	rec := tripRequest(t, handler, http.MethodGet, "/api/trips/"+tripTestID, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), tripTestParticipant) {
		t.Fatalf("the roster leaked a participant user id: %s", rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	participants, ok := body["participants"].([]any)
	if !ok || len(participants) != 1 {
		t.Fatalf("participants = %v, want one entry", body["participants"])
	}
	row, _ := participants[0].(map[string]any)
	for _, key := range []string{"participantId", "name", "userIsSelf", "addedByName"} {
		if _, present := row[key]; !present {
			t.Errorf("roster row is missing %q: %v", key, row)
		}
	}
	if len(row) != 4 {
		t.Errorf("roster row carries %d keys, want exactly the four the contract declares "+
			"(MYR-618 added addedByName): %v", len(row), row)
	}
	// The CALLER is the owner, not the participant, so userIsSelf is false —
	// which also proves the flag is computed rather than hard-coded true.
	if row["userIsSelf"] != false {
		t.Errorf("userIsSelf = %v, want false for a roster row that is not the caller", row["userIsSelf"])
	}
}

// TestLeaveIsSilentAndIdempotent pins the one route that answers 204 to
// everything.
//
// It must report nothing about whether the trip exists or whether the caller
// was on it — a 404 for "not a member" would tell any authenticated caller
// which trip ids are real, which is exactly what the rest of the surface
// refuses to do. And there is nothing a client would do differently with the
// distinction: it wanted to not be on the trip, and it is not on the trip.
func TestLeaveIsSilentAndIdempotent(t *testing.T) {
	store := &fakeTripStore{trip: fixtureTrip()}
	handler := newTripTestHandler(t, store, true)

	for i := 1; i <= 2; i++ {
		rec := tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID+"/participants/me", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("call %d: status = %d, want 204. Body: %s", i, rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Errorf("call %d: 204 carried a body: %s", i, rec.Body.String())
		}
	}
	if store.leaveCalls != 2 {
		t.Fatalf("store saw %d leave calls, want 2 — the handler must reach the store both times", store.leaveCalls)
	}
}

// TestLeaveStillReportsATransportFailure is the other half of the rule above.
//
// 204 is the answer to "you are not on this trip", NOT to "the database is
// down". Answering 204 on a failed write would tell the caller they had left
// when they had not — and they would keep receiving the car's location.
func TestLeaveStillReportsATransportFailure(t *testing.T) {
	handler := newTripTestHandler(t, &fakeTripStore{err: errors.New("connection reset")}, true)
	rec := tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID+"/participants/me", "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500. Body: %s", rec.Code, rec.Body.String())
	}
}

// TestActivityTokenIsNeverEchoed pins the P1 capability rule.
//
// Whoever holds the token plus the team's APNs key can write to that phone's
// lock screen. The 204 carries no body at all, which is the strongest form of
// "the caller already knows what it sent".
func TestActivityTokenIsNeverEchoed(t *testing.T) {
	const token = "8f3a91c0deadbeefcafef00dfeedface" //nolint:gosec // G101: a fixed test fixture, not a credential.

	store := &fakeTripStore{trip: fixtureTrip()}
	handler := newTripTestHandler(t, store, true)

	rec := tripRequest(t, handler, http.MethodPost, "/api/trips/"+tripTestID+"/activity-start-token",
		`{"pushToStartToken":"`+token+`","sandbox":true}`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204. Body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 carried a body: %s", rec.Body.String())
	}
	if store.tokenCalls != 1 {
		t.Fatalf("store saw %d registrations, want 1", store.tokenCalls)
	}
}

// TestActivityTokenRegistrationRequiresMembership pins that registering is a
// grant of permission and therefore gated.
//
// Registering a token authorises the server to write to this phone's lock
// screen ABOUT THIS TRIP, so it must be a trip the caller is on — and the
// refusal is 404, so the endpoint is not an oracle for trip ids either.
func TestActivityTokenRegistrationRequiresMembership(t *testing.T) {
	store := &fakeTripStore{err: ErrTripNotFound}
	handler := newTripTestHandler(t, store, true)

	rec := tripRequest(t, handler, http.MethodPost, "/api/trips/"+tripTestID+"/activity-start-token",
		`{"pushToStartToken":"abc123"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. Body: %s", rec.Code, rec.Body.String())
	}
	if store.tokenCalls != 0 {
		t.Fatalf("the token was stored despite the membership refusal")
	}
}

// TestActivityTokenDeleteNeedsNoMembership pins the deliberate asymmetry with
// the POST.
//
// DELETE only ever removes the CALLER'S OWN row, so the worst a stranger
// achieves is deleting a row they do not have. Requiring a membership read
// would add a 404 that tells them whether the trip is real, for a call that
// changes nothing either way — and would lock out the person who most needs it:
// a participant who has just LEFT and no longer passes the read.
func TestActivityTokenDeleteNeedsNoMembership(t *testing.T) {
	store := &fakeTripStore{err: ErrTripNotFound}
	handler := newTripTestHandler(t, store, true)

	tripRequest(t, handler, http.MethodDelete, "/api/trips/"+tripTestID+"/activity-start-token", "")

	// The scripted error reaches the DELETE call itself, so this asserts the
	// handler did not refuse BEFORE the store: a membership pre-check would
	// have produced a 404 with deleteCalls still 0.
	if store.deleteCalls != 1 {
		t.Fatalf("store saw %d deletes, want 1 — the handler must not pre-check membership", store.deleteCalls)
	}
}

// TestListRefusesAnUnknownStatusRatherThanWidening.
//
// A client that asked for `?status=activE` and received EVERY trip would render
// the wrong list and never learn why. Silently widening a filter is the failure
// a typed enum exists to prevent.
func TestListRefusesAnUnknownStatusRatherThanWidening(t *testing.T) {
	handler := newTripTestHandler(t, &fakeTripStore{trip: fixtureTrip()}, true)

	rec := tripRequest(t, handler, http.MethodGet, "/api/trips?status=activE", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. Body: %s", rec.Code, rec.Body.String())
	}
	if got := decodeTripError(t, rec).Code; got != wserrors.ErrCodeInvalidRequest {
		t.Errorf("code = %q, want invalid_request", got)
	}
}

// TestListEnvelopeCarriesNoCursor pins a contract statement rather than an
// omission: a person has a handful of trips, not a feed, and an SDK pagination
// helper must not mistake this for a page and go looking for a cursor that will
// never be there.
func TestListEnvelopeCarriesNoCursor(t *testing.T) {
	handler := newTripTestHandler(t, &fakeTripStore{trip: fixtureTrip()}, true)
	rec := tripRequest(t, handler, http.MethodGet, "/api/trips", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("envelope has %d keys, want exactly `items`: %v", len(body), body)
	}
	if _, ok := body["items"].([]any); !ok {
		t.Fatalf("items is not an array: %v", body["items"])
	}
}

// TestUnknownBodyFieldsAreRefused pins the strict decode. A client sending a
// field this server version does not know finds out, rather than having it
// silently dropped and wondering why the trip has the wrong dates.
func TestUnknownBodyFieldsAreRefused(t *testing.T) {
	handler := newTripTestHandler(t, &fakeTripStore{trip: fixtureTrip()}, true)

	rec := tripRequest(t, handler, http.MethodPatch, "/api/trips/"+tripTestID,
		`{"unknownField":"typo"}`) //nolint:misspell // the point is a field the server does not know
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. Body: %s", rec.Code, rec.Body.String())
	}
}

// ── The TripNotifier seam ────────────────────────────────────────────────────
//
// cmd/telemetry-server wires the real `trips` push category behind this
// interface, through an adapter over internal/trips' Service.
// These pin the CONTRACT it will be called under: who is told, when, and — just
// as load-bearing — who is NOT.

// TestNotifierIsOptionalAndNilIsSilent pins the property the whole seam rests
// on: a deployment with no notifier creates trips that work perfectly and tells
// nobody. A push is an announcement about a state change, never the state
// change itself, so an absent notifier must not be able to fail a create.
func TestNotifierIsOptionalAndNilIsSilent(t *testing.T) {
	handler := newTripTestHandler(t, &fakeTripStore{trip: fixtureTrip()}, true)

	rec := tripRequest(t, handler, http.MethodPost, "/api/vehicles/"+tripTestVehicle+"/trips",
		`{"name":"Trip","startsAt":"2026-09-06T00:00:00Z","endsAt":"2026-09-08T00:00:00Z","participantIds":[]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 with no notifier wired. Body: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateNotifiesParticipantsAndNotTheOwner.
//
// THE OWNER IS DELIBERATELY NOT IN THE FAN-OUT. All three REST-caused events
// announce something the owner just did, and a phone that buzzes to tell its
// owner about their own tap is the most common way a notification category gets
// switched off. The owner IS included in the per-leg Live Activity, which is a
// different mechanism answering a different question.
func TestCreateNotifiesParticipantsAndNotTheOwner(t *testing.T) {
	notifier := &recordingTripNotifier{}
	handler := newTripTestHandler(t, &fakeTripStore{trip: fixtureTrip()}, true, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodPost, "/api/vehicles/"+tripTestVehicle+"/trips",
		`{"name":"Trip","startsAt":"2026-09-06T00:00:00Z","endsAt":"2026-09-08T00:00:00Z","participantIds":["csh_1"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. Body: %s", rec.Code, rec.Body.String())
	}

	if len(notifier.added) != 1 {
		t.Fatalf("trip_added fired %d times, want once", len(notifier.added))
	}
	got := notifier.added[0]
	if len(got) != 1 || got[0] != tripTestParticipant {
		t.Fatalf("trip_added went to %v, want exactly the participant", got)
	}

	// The fixture's window is ALREADY OPEN, which is the common case for a road
	// trip already underway — so `trip_started` is owed immediately rather than
	// at a boundary the sweeper would have to wait for.
	if len(notifier.started) != 1 {
		t.Fatalf("trip_started fired %d times on an already-open window, want once", len(notifier.started))
	}
}

// TestCreateOfAScheduledTripDoesNotAnnounceAStart. A window that has not opened
// yet starts at an instant no request is present for, and the sweeper owns that
// transition — its `started_notified_at` stamp is what stops the two of them
// sending it twice.
func TestCreateOfAScheduledTripDoesNotAnnounceAStart(t *testing.T) {
	trip := fixtureTrip()
	trip.StartsAt = time.Now().Add(48 * time.Hour)
	trip.EndsAt = trip.StartsAt.Add(72 * time.Hour)

	notifier := &recordingTripNotifier{}
	handler := newTripTestHandler(t, &fakeTripStore{trip: trip}, true, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodPost, "/api/vehicles/"+tripTestVehicle+"/trips",
		`{"name":"Trip","startsAt":"2026-09-08T00:00:00Z","endsAt":"2026-09-11T00:00:00Z","participantIds":["csh_1"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. Body: %s", rec.Code, rec.Body.String())
	}
	if len(notifier.added) != 1 {
		t.Errorf("trip_added fired %d times, want once — a scheduled trip still tells its participants", len(notifier.added))
	}
	if len(notifier.started) != 0 {
		t.Fatalf("trip_started fired for a window that has not opened: %v", notifier.started)
	}
}

// TestPatchNotifiesOnlyTheNewArrivals.
//
// Sending `trip_added` to the whole roster on every patch would re-notify
// everybody already on the trip, which reads as the trip having been created a
// second time.
func TestPatchNotifiesOnlyTheNewArrivals(t *testing.T) {
	before := fixtureTrip()
	after := fixtureTrip()
	after.Participants = append(after.Participants,
		TripParticipantData{ParticipantID: "csh_2", Name: "Joey", UserID: "usr_joey"})

	// The fake returns `after` from BOTH the pre-read and the update, which is
	// the harder case: the handler must still compute the delta from the two
	// calls it makes rather than assuming the pre-read is stale.
	store := &tripPatchStore{before: before, after: after}
	notifier := &recordingTripNotifier{}
	handler := newTripTestHandler(t, store, true, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodPatch, "/api/trips/"+tripTestID, `{"addParticipantIds":["csh_2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	if len(notifier.added) != 1 {
		t.Fatalf("trip_added fired %d times, want once", len(notifier.added))
	}
	got := notifier.added[0]
	if len(got) != 1 || got[0] != "usr_joey" {
		t.Fatalf("trip_added went to %v, want only the new arrival", got)
	}
}

// TestRemovingAParticipantAnnouncesNothing is a decision, not an omission. The
// contract lists five `trips` events and none of them is "you were removed";
// the person's live access ends the moment the row is stamped, and announcing
// it would be telling somebody they had been taken off a trip by a person who
// chose not to tell them.
func TestRemovingAParticipantAnnouncesNothing(t *testing.T) {
	before := fixtureTrip()
	after := fixtureTrip()
	after.Participants = nil

	store := &tripPatchStore{before: before, after: after}
	notifier := &recordingTripNotifier{}
	handler := newTripTestHandler(t, store, true, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodPatch, "/api/trips/"+tripTestID, `{"removeParticipantIds":["csh_1"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	if len(notifier.added) != 0 || len(notifier.ended) != 0 {
		t.Fatalf("a removal announced something: added=%v ended=%v", notifier.added, notifier.ended)
	}
}

// TestEndingAnAlreadyEndedTripAnnouncesNothing pins the idempotency of the
// ANNOUNCEMENT, which is separate from the idempotency of the write. The first
// call already told everybody; a second announcement about the same fact is how
// a notification category gets turned off.
func TestEndingAnAlreadyEndedTripAnnouncesNothing(t *testing.T) {
	ended := fixtureTrip()
	endedAt := time.Now().Add(-time.Hour)
	ended.EndedAt = &endedAt

	notifier := &recordingTripNotifier{}
	handler := newTripTestHandler(t, &fakeTripStore{trip: ended}, true, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodPost, "/api/trips/"+tripTestID+"/end", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want an idempotent 200. Body: %s", rec.Code, rec.Body.String())
	}
	if len(notifier.ended) != 0 {
		t.Fatalf("trip_ended fired for a trip that had already ended: %v", notifier.ended)
	}
}

// TestEndingALiveTripAnnouncesItToTheParticipants is the positive half.
func TestEndingALiveTripAnnouncesItToTheParticipants(t *testing.T) {
	notifier := &recordingTripNotifier{}
	handler := newTripTestHandler(t, &fakeTripStore{trip: fixtureTrip()}, true, WithTripNotifier(notifier))

	rec := tripRequest(t, handler, http.MethodPost, "/api/trips/"+tripTestID+"/end", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	if len(notifier.ended) != 1 {
		t.Fatalf("trip_ended fired %d times, want once", len(notifier.ended))
	}
	if got := notifier.ended[0]; len(got) != 1 || got[0] != tripTestParticipant {
		t.Fatalf("trip_ended went to %v, want exactly the participant", got)
	}
}

// tripPatchStore returns a different trip from the pre-read than from the
// update, so the delta the notifier fan-out computes is observable.
type tripPatchStore struct {
	fakeTripStore
	before TripData
	after  TripData
	reads  int
}

func (s *tripPatchStore) GetTrip(context.Context, string, string) (TripData, error) {
	s.reads++
	return s.before, nil
}

func (s *tripPatchStore) UpdateTrip(context.Context, string, string, TripUpdateInput) (TripData, error) {
	return s.after, nil
}
