package fleetrepush

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ── Test doubles ────────────────────────────────────────────────────────────

type fakeStore struct {
	rows      []Candidate
	err       error
	gotLimit  int
	callCount int
}

func (f *fakeStore) ListStreamingFleetConfigVehicles(_ context.Context, limit int) ([]Candidate, error) {
	f.callCount++
	f.gotLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	// Mirror the store's own LIMIT so a cap test exercises the same truncation
	// the database would perform.
	if limit < len(f.rows) {
		return f.rows[:limit], nil
	}
	return f.rows, nil
}

type fakeTokens struct {
	byUser map[string]string
	errs   map[string]error
	calls  []string
}

func (f *fakeTokens) AccessToken(_ context.Context, userID string) (string, error) {
	f.calls = append(f.calls, userID)
	if err := f.errs[userID]; err != nil {
		return "", err
	}
	if tok, ok := f.byUser[userID]; ok {
		return tok, nil
	}
	return "tok-" + userID, nil
}

type fakeReader struct {
	status map[string]ConfigStatus
	errs   map[string]error
	calls  []string
}

func (f *fakeReader) GetTelemetryConfig(_ context.Context, _, vin string) (ConfigStatus, error) {
	f.calls = append(f.calls, vin)
	if err := f.errs[vin]; err != nil {
		return ConfigStatus{}, err
	}
	if st, ok := f.status[vin]; ok {
		return st, nil
	}
	return ConfigStatus{Configured: true}, nil
}

type fakePusher struct {
	pushed []string
	errs   map[string]error
	// onPush fires after a successful push, so a test can cancel the run at a
	// deterministic point rather than racing a timer.
	onPush func()
}

func (f *fakePusher) PushForVIN(_ context.Context, _, vin string) error {
	if err := f.errs[vin]; err != nil {
		return err
	}
	f.pushed = append(f.pushed, vin)
	if f.onPush != nil {
		f.onPush()
	}
	return nil
}

// errMissingKey stands in for telemetry's *SkippedVehicleError.
var errMissingKey = errors.New("skipped: missing_key")

type fakeClassifier struct{}

func (fakeClassifier) IsAwaitingVirtualKey(err error) bool { return errors.Is(err, errMissingKey) }

type fakeAuditor struct {
	users []string
	err   error
}

func (f *fakeAuditor) RecordTokenDecrypt(_ context.Context, userID string) error {
	if f.err != nil {
		return f.err
	}
	f.users = append(f.users, userID)
	return nil
}

// harness wires a sweep over doubles with the rate limiter disabled.
type harness struct {
	store    *fakeStore
	tokens   *fakeTokens
	reader   *fakeReader
	pusher   *fakePusher
	auditor  *fakeAuditor
	deps     Deps
	interval time.Duration
}

func newHarness(rows ...Candidate) *harness {
	h := &harness{
		store:   &fakeStore{rows: rows},
		tokens:  &fakeTokens{byUser: map[string]string{}, errs: map[string]error{}},
		reader:  &fakeReader{status: map[string]ConfigStatus{}, errs: map[string]error{}},
		pusher:  &fakePusher{errs: map[string]error{}},
		auditor: &fakeAuditor{},
		// Negative disables the wait: a unit test must not spend a second per
		// vehicle proving the pacing constant.
		interval: -1,
	}
	h.deps = Deps{
		Store:    h.store,
		Tokens:   h.tokens,
		Reader:   h.reader,
		Pusher:   h.pusher,
		Classify: fakeClassifier{},
		Auditor:  h.auditor,
	}
	return h
}

func (h *harness) run(t *testing.T, cfg Config) Report {
	t.Helper()
	cfg.Interval = h.interval
	rep, err := New(h.deps, cfg, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func candidate(vin, user string) Candidate {
	return Candidate{VehicleID: "veh_" + vin, VIN: vin, UserID: user, LastUpdated: time.Now()}
}

// ── The dry-run / apply split ───────────────────────────────────────────────

// THE PROPERTY THE WHOLE TOOL RESTS ON: the default run must not touch a single
// car. An operator reads the dry run to decide whether to spend the pushes.
func TestSweep_DryRunPushesNothing(t *testing.T) {
	h := newHarness(candidate("VIN00000000000001", "u1"), candidate("VIN00000000000002", "u2"))

	rep := h.run(t, Config{})

	if len(h.pusher.pushed) != 0 {
		t.Fatalf("dry run pushed %v — it must push nothing", h.pusher.pushed)
	}
	if rep.Mode != "dry-run" {
		t.Errorf("Mode = %q, want dry-run", rep.Mode)
	}
	if rep.WouldPush != 2 || rep.Pushed != 0 {
		t.Errorf("wouldPush=%d pushed=%d, want 2 and 0", rep.WouldPush, rep.Pushed)
	}
	if len(h.reader.calls) != 2 {
		t.Errorf("config reads = %d, want 2 — the dry run still reports real config state", len(h.reader.calls))
	}
	for _, v := range rep.Vehicles {
		if v.Action != ActionWouldPush {
			t.Errorf("%s: action = %q, want %q", v.VIN, v.Action, ActionWouldPush)
		}
	}
}

func TestSweep_ApplyPushesEveryEligibleCar(t *testing.T) {
	h := newHarness(candidate("VIN00000000000001", "u1"), candidate("VIN00000000000002", "u2"))

	rep := h.run(t, Config{Apply: true})

	if len(h.pusher.pushed) != 2 {
		t.Fatalf("pushed %v, want both VINs", h.pusher.pushed)
	}
	if rep.Mode != "apply" {
		t.Errorf("Mode = %q, want apply", rep.Mode)
	}
	if rep.Pushed != 2 || rep.WouldPush != 0 {
		t.Errorf("pushed=%d wouldPush=%d, want 2 and 0", rep.Pushed, rep.WouldPush)
	}
}

// Re-running is the supported way to finish a sweep, so it must be boring:
// the same set pushed again, no state carried between runs.
func TestSweep_ApplyIsIdempotentOnRerun(t *testing.T) {
	h := newHarness(candidate("VIN00000000000001", "u1"))

	first := h.run(t, Config{Apply: true})
	second := h.run(t, Config{Apply: true})

	if first.Pushed != second.Pushed || first.Skipped != second.Skipped || first.Failed != second.Failed {
		t.Fatalf("re-run differed: %+v vs %+v", first, second)
	}
	if len(h.pusher.pushed) != 2 {
		t.Fatalf("pushed %v, want the same VIN once per run", h.pusher.pushed)
	}
}

// ── The cap ─────────────────────────────────────────────────────────────────

func TestSweep_LimitCapsOneRun(t *testing.T) {
	rows := make([]Candidate, 0, 5)
	for _, vin := range []string{"VIN1", "VIN2", "VIN3", "VIN4", "VIN5"} {
		rows = append(rows, candidate(vin, "u1"))
	}
	h := newHarness(rows...)

	rep := h.run(t, Config{Apply: true, Limit: 2})

	if h.store.gotLimit != 2 {
		t.Errorf("store asked for limit %d, want 2 — the cap must bound the QUERY, not just the loop",
			h.store.gotLimit)
	}
	if len(h.pusher.pushed) != 2 {
		t.Fatalf("pushed %v, want 2", h.pusher.pushed)
	}
	if rep.Examined != 2 || rep.Limit != 2 {
		t.Errorf("examined=%d limit=%d, want 2 and 2", rep.Examined, rep.Limit)
	}
}

func TestSweep_ZeroLimitUsesTheDefault(t *testing.T) {
	h := newHarness(candidate("VIN00000000000001", "u1"))

	rep := h.run(t, Config{})

	if h.store.gotLimit != DefaultLimit || rep.Limit != DefaultLimit {
		t.Errorf("limit = %d/%d, want DefaultLimit %d", h.store.gotLimit, rep.Limit, DefaultLimit)
	}
}

// ── Skip reasons ────────────────────────────────────────────────────────────

// Each of these is a car the sweep must REFUSE, and refuse for a stated reason:
// a silent drop makes "pushed 6 of 9" unexplainable.
func TestSweep_SkipReasons(t *testing.T) {
	suspended := candidate("VINSUSPENDED00001", "u_susp")
	suspended.Suspended = true
	absent := candidate("VINABSENT00000001", "u_abs")
	absent.ConfigAbsent = true
	pendingAck := candidate("VINPENDINGACK0001", "u_ack")
	pendingAck.PendingOwnerAck = true

	tests := []struct {
		name       string
		row        Candidate
		arrange    func(*harness)
		wantReason string
		wantTesla  bool // did any Tesla call happen for this car?
	}{
		{
			name:       "owner suspended for inactivity",
			row:        suspended,
			wantReason: ReasonOwnerSuspended,
		},
		{
			name:       "config never landed",
			row:        absent,
			wantReason: ReasonConfigAbsent,
		},
		{
			name:       "driver car awaiting owner acknowledgment",
			row:        pendingAck,
			wantReason: ReasonAwaitingOwnerAck,
		},
		{
			name: "no tesla token on file",
			row:  candidate("VINNOTOKEN0000001", "u_notok"),
			arrange: func(h *harness) {
				h.tokens.errs["u_notok"] = ErrNoToken
			},
			wantReason: ReasonNoToken,
		},
		{
			name: "tesla reports nothing configured",
			row:  candidate("VINNOCONFIG000001", "u_nocfg"),
			arrange: func(h *harness) {
				h.reader.status["VINNOCONFIG000001"] = ConfigStatus{Configured: false}
			},
			wantReason: ReasonNoConfig,
			wantTesla:  true,
		},
		{
			name: "tesla 404s the config read",
			row:  candidate("VIN404000000000001", "u_404"),
			arrange: func(h *harness) {
				h.reader.errs["VIN404000000000001"] = ErrNoConfig
			},
			wantReason: ReasonNoConfig,
			wantTesla:  true,
		},
		{
			name: "virtual key not enrolled",
			row:  candidate("VINMISSINGKEY0001", "u_key"),
			arrange: func(h *harness) {
				h.pusher.errs["VINMISSINGKEY0001"] = errMissingKey
			},
			wantReason: ReasonMissingKey,
			wantTesla:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(tt.row)
			if tt.arrange != nil {
				tt.arrange(h)
			}

			rep := h.run(t, Config{Apply: true})

			if rep.Skipped != 1 || rep.Pushed != 0 {
				t.Fatalf("skipped=%d pushed=%d, want 1 and 0", rep.Skipped, rep.Pushed)
			}
			if got := rep.Vehicles[0].Reason; got != tt.wantReason {
				t.Errorf("reason = %q, want %q", got, tt.wantReason)
			}
			if rep.SkipReasons[tt.wantReason] != 1 {
				t.Errorf("skipReasons = %v, want one %s", rep.SkipReasons, tt.wantReason)
			}
			if got := len(h.reader.calls) > 0; got != tt.wantTesla {
				t.Errorf("Tesla read happened = %v, want %v — a row-shaped refusal must cost nothing", got, tt.wantTesla)
			}
		})
	}
}

// A row-shaped refusal must not even reach the owner's credential: no decrypt,
// no audit row, nothing to attribute.
func TestSweep_RowShapedRefusalNeverTouchesTheToken(t *testing.T) {
	suspended := candidate("VINSUSPENDED00001", "u_susp")
	suspended.Suspended = true
	h := newHarness(suspended)

	h.run(t, Config{Apply: true})

	if len(h.tokens.calls) != 0 {
		t.Errorf("token resolved for %v — a suspended car must cost no decrypt", h.tokens.calls)
	}
	if len(h.auditor.users) != 0 {
		t.Errorf("audit row written for %v — nothing was decrypted", h.auditor.users)
	}
}

// ── Failures ────────────────────────────────────────────────────────────────

func TestSweep_FailureReasons(t *testing.T) {
	tests := []struct {
		name       string
		arrange    func(*harness)
		wantReason string
	}{
		{
			name:       "token refresh failed",
			arrange:    func(h *harness) { h.tokens.errs["u1"] = errors.New("tesla token expired") },
			wantReason: ReasonTokenFailed,
		},
		{
			name:       "config read failed",
			arrange:    func(h *harness) { h.reader.errs["VIN00000000000001"] = errors.New("503") },
			wantReason: ReasonConfigReadFailed,
		},
		{
			name:       "push failed",
			arrange:    func(h *harness) { h.pusher.errs["VIN00000000000001"] = errors.New("500") },
			wantReason: ReasonPushFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(candidate("VIN00000000000001", "u1"))
			tt.arrange(h)

			rep := h.run(t, Config{Apply: true})

			if rep.Failed != 1 || rep.Pushed != 0 {
				t.Fatalf("failed=%d pushed=%d, want 1 and 0", rep.Failed, rep.Pushed)
			}
			if got := rep.Vehicles[0].Reason; got != tt.wantReason {
				t.Errorf("reason = %q, want %q", got, tt.wantReason)
			}
			if rep.Vehicles[0].Error == "" {
				t.Error("failed line carries no error text; the operator cannot act on it")
			}
			if rep.FailureReasons[tt.wantReason] != 1 {
				t.Errorf("failureReasons = %v, want one %s", rep.FailureReasons, tt.wantReason)
			}
		})
	}
}

// One car's failure must not end the run — the fleet behind it still needs the
// new field set.
func TestSweep_OneFailureDoesNotStopTheRun(t *testing.T) {
	h := newHarness(
		candidate("VIN00000000000001", "u1"),
		candidate("VIN00000000000002", "u2"),
	)
	h.pusher.errs["VIN00000000000001"] = errors.New("500")

	rep := h.run(t, Config{Apply: true})

	if rep.Failed != 1 || rep.Pushed != 1 {
		t.Fatalf("failed=%d pushed=%d, want 1 and 1", rep.Failed, rep.Pushed)
	}
}

// ── The MYR-447 audit ───────────────────────────────────────────────────────

func TestSweep_AuditsEachOwnerOnceBeforeTheTokenIsRead(t *testing.T) {
	h := newHarness(
		candidate("VIN00000000000001", "u1"),
		candidate("VIN00000000000002", "u1"), // same owner, second car
		candidate("VIN00000000000003", "u2"),
	)

	h.run(t, Config{Apply: true})

	if len(h.auditor.users) != 2 {
		t.Fatalf("audit rows = %v, want one per OWNER (u1, u2)", h.auditor.users)
	}
	if h.auditor.users[0] != "u1" || h.auditor.users[1] != "u2" {
		t.Errorf("audit rows = %v, want [u1 u2]", h.auditor.users)
	}
}

// An unattributable decrypt is the thing the audit exists to prevent, so a
// failed audit write aborts the run rather than continuing to produce more.
func TestSweep_AuditFailureAbortsTheRun(t *testing.T) {
	h := newHarness(
		candidate("VIN00000000000001", "u1"),
		candidate("VIN00000000000002", "u2"),
	)
	h.auditor.err = errors.New("audit table unavailable")

	_, err := New(h.deps, Config{Apply: true, Interval: -1}, nil).Run(context.Background())

	if err == nil {
		t.Fatal("Run returned nil; a failed audit write must abort")
	}
	if len(h.tokens.calls) != 0 {
		t.Errorf("token resolved for %v after the audit write failed", h.tokens.calls)
	}
	if len(h.pusher.pushed) != 0 {
		t.Errorf("pushed %v after the audit write failed", h.pusher.pushed)
	}
}

// ── Config age ──────────────────────────────────────────────────────────────

// The age is the only evidence an operator has that a config predates a field
// change — and, after an --apply, that it no longer does.
func TestSweep_ConfigAgeFromEchoedExp(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	// A config pushed 100 days ago expires 250 days from now.
	exp := now.Add(250 * 24 * time.Hour).Unix()
	fresh := now.Add(350 * 24 * time.Hour).Unix()

	h := newHarness(
		candidate("VINOLD00000000001", "u1"),
		candidate("VINNEW00000000001", "u1"),
		candidate("VINUNKNOWN0000001", "u1"),
	)
	h.reader.status["VINOLD00000000001"] = ConfigStatus{Configured: true, Exp: &exp}
	h.reader.status["VINNEW00000000001"] = ConfigStatus{Configured: true, Exp: &fresh}
	h.reader.status["VINUNKNOWN0000001"] = ConfigStatus{Configured: true}

	rep := h.run(t, Config{Now: func() time.Time { return now }})

	byVIN := map[string]VehicleReport{}
	for _, v := range rep.Vehicles {
		byVIN[v.VIN] = v
	}
	if got := byVIN["VINOLD00000000001"].ConfigAgeDays; got == nil || *got < 99.9 || *got > 100.1 {
		t.Errorf("old config age = %v, want ~100 days", got)
	}
	if got := byVIN["VINNEW00000000001"].ConfigAgeDays; got == nil || *got > 0.01 {
		t.Errorf("just-pushed config age = %v, want ~0", got)
	}
	if got := byVIN["VINUNKNOWN0000001"].ConfigAgeDays; got != nil {
		t.Errorf("age = %v with no echoed exp, want nil (unknown, not zero)", got)
	}
}

// ── Pacing ──────────────────────────────────────────────────────────────────

// The rate limit must not charge for the first call, or a one-car sweep waits
// for nothing.
func TestSweep_PacingWaitsBetweenCallsNotBeforeTheFirst(t *testing.T) {
	h := newHarness(candidate("VIN00000000000001", "u1"))
	h.interval = 50 * time.Millisecond

	start := time.Now()
	rep := h.run(t, Config{})
	elapsed := time.Since(start)

	if rep.WouldPush != 1 {
		t.Fatalf("wouldPush = %d, want 1", rep.WouldPush)
	}
	if elapsed >= 50*time.Millisecond {
		t.Errorf("one-vehicle dry run took %s; the first call must not wait", elapsed)
	}
}

func TestSweep_CancelledContextStopsTheRun(t *testing.T) {
	h := newHarness(
		candidate("VIN00000000000001", "u1"),
		candidate("VIN00000000000002", "u2"),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel the instant the first car is done, so the second is refused at its
	// next Tesla call and nothing depends on a sleep.
	h.pusher.onPush = cancel

	rep, err := New(h.deps, Config{Apply: true, Interval: -1}, nil).Run(ctx)

	if err == nil {
		t.Fatal("Run returned nil on a cancelled context")
	}
	// The partial report still has to name what was already changed.
	if rep.Pushed != 1 {
		t.Errorf("pushed = %d, want the 1 car finished before cancellation to be reported", rep.Pushed)
	}
}

// The store's own error must surface rather than reading as an empty fleet.
func TestSweep_StoreErrorFailsTheRun(t *testing.T) {
	h := newHarness()
	h.store.err = errors.New("connection refused")

	_, err := New(h.deps, Config{Interval: -1}, nil).Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil when the candidate listing failed")
	}
}
