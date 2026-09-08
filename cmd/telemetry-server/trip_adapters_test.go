package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// TestTranslateTripErrorCoversEveryStoreSentinel is the exhaustiveness gate on
// the one place the store's error vocabulary meets the handler's.
//
// AN UNMAPPED SENTINEL IS NOT A COMPILE ERROR — it falls through to the default
// arm and is reported as 500, which tells a client to RETRY a request that will
// never succeed. That is the failure this test exists to make impossible, and
// it is why the table below is written as "every sentinel the store exports"
// rather than "the ones I remembered".
//
// The store's sentinels are unexported as a set, so the list is spelled out
// here; TestTripSentinelListIsComplete below pins that the spelling is total by
// checking the count against the package's own declaration site.
func TestTranslateTripErrorCoversEveryStoreSentinel(t *testing.T) {
	cases := []struct {
		name  string
		store error
		want  error
	}{
		{"not found", store.ErrTripNotFound, telemetry.ErrTripNotFound},
		{"overlap", store.ErrTripOverlap, telemetry.ErrTripOverlaps},
		{"participant not shared", store.ErrTripParticipantNotShared, telemetry.ErrTripParticipantNotShared},
		{"window invalid", store.ErrTripWindowInvalid, telemetry.ErrTripWindowInvalid},
		{"name invalid", store.ErrTripNameInvalid, telemetry.ErrTripNameInvalid},
		{"already ended", store.ErrTripEnded, telemetry.ErrTripEnded},
		{"owner removed them", store.ErrTripParticipantOwnerRemoved, telemetry.ErrTripParticipantOwnerRemoved},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// WRAPPED, as the repository actually returns them — a translation
			// that only matched a bare sentinel would work in this test and
			// fail on every real call, because every store method wraps with
			// its own context.
			wrapped := fmt.Errorf("TripRepo.Something(x): %w", tc.store)

			got := translateTripError(wrapped)
			if !errors.Is(got, tc.want) {
				t.Fatalf("translate(%v) = %v, want it to wrap %v", wrapped, got, tc.want)
			}
			// THE ORIGINAL SURVIVES IN THE CHAIN, so the server-side log still
			// names the underlying cause while the handler branches on its own
			// sentinel.
			if !errors.Is(got, tc.store) {
				t.Errorf("translate(%v) lost the store cause", wrapped)
			}
		})
	}
}

// TestTranslateTripErrorPassesThroughAnUnknownError.
//
// A transport failure or a bug must NOT be dressed as one of the refusals: that
// would tell a client to change a request that was fine. It is passed through
// unchanged so the handler's default arm reports 500 and logs it.
func TestTranslateTripErrorPassesThroughAnUnknownError(t *testing.T) {
	boom := errors.New("connection reset by peer")

	got := translateTripError(boom)
	if !errors.Is(got, boom) {
		t.Fatalf("translate lost the original error: %v", got)
	}
	for _, sentinel := range []error{
		telemetry.ErrTripNotFound,
		telemetry.ErrTripOverlaps,
		telemetry.ErrTripParticipantNotShared,
		telemetry.ErrTripWindowInvalid,
		telemetry.ErrTripNameInvalid,
		telemetry.ErrTripEnded,
	} {
		if errors.Is(got, sentinel) {
			t.Errorf("an unknown error was translated into %v", sentinel)
		}
	}
}

// TestTranslateTripErrorPassesNilThrough. The adapters call it unconditionally
// on every return path, so a nil that came back non-nil would turn every
// successful trip call into a 500.
func TestTranslateTripErrorPassesNilThrough(t *testing.T) {
	if err := translateTripError(nil); err != nil {
		t.Fatalf("translate(nil) = %v, want nil", err)
	}
}

// TestTripSentinelListIsComplete makes the table above TOTAL rather than
// merely long.
//
// It reads the store package's own declaration site and requires every
// `ErrTrip*` sentinel to be either TRANSLATED or explicitly EXEMPTED with a
// reason. Adding a seventh sentinel upstream therefore fails here — which is
// the whole point, because the alternative is that it falls through to the
// default arm and a client is told to retry something that cannot succeed.
func TestTripSentinelListIsComplete(t *testing.T) {
	// The sentinels this adapter deliberately does NOT translate, each with the
	// reason it never reaches a handler.
	exempt := map[string]string{
		"ErrTripLegOpen": "the leg detector's idempotency guard on a redelivered " +
			"drive-start. It is handled as a NO-OP where it is raised and is never " +
			"returned through a REST call, so a wire mapping for it would describe " +
			"a path that does not exist.",
	}

	declared := tripSentinelNames(t)
	if len(declared) == 0 {
		t.Fatal("read zero ErrTrip* sentinels from internal/store — the scan is broken")
	}

	translated := map[string]bool{
		"ErrTripNotFound": true, "ErrTripOverlap": true,
		"ErrTripParticipantNotShared": true, "ErrTripWindowInvalid": true,
		"ErrTripNameInvalid": true, "ErrTripEnded": true,
		"ErrTripParticipantOwnerRemoved": true,
	}
	for _, name := range declared {
		if translated[name] {
			continue
		}
		if _, ok := exempt[name]; ok {
			continue
		}
		t.Errorf("store.%s is neither translated by translateTripError nor exempted — "+
			"an unmapped sentinel is reported as 500, which tells a client to retry a "+
			"request that will never succeed", name)
	}
	for name := range translated {
		if !slicesContains(declared, name) {
			t.Errorf("translateTripError maps store.%s, which no longer exists", name)
		}
	}
}

// tripSentinelNames scans internal/store for `ErrTrip*` sentinel declarations.
//
// A SOURCE SCAN rather than reflection, because the sentinels are package-level
// vars of type `error` and Go offers no way to enumerate them at runtime. The
// scan is deliberately dumb — it matches the declaration prefix — so it cannot
// be fooled by a sentinel added in a different file.
func tripSentinelNames(t *testing.T) []string {
	t.Helper()

	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "internal", "store"))
	if err != nil {
		t.Fatalf("read internal/store: %v", err)
	}

	var names []string
	re := regexp.MustCompile(`(?m)^\s*(ErrTrip\w*)\s*=\s*errors\.New\(`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(root, "internal", "store", e.Name())) //nolint:gosec // test-only read of a repo-relative source file
		if readErr != nil {
			t.Fatalf("read %s: %v", e.Name(), readErr)
		}
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			names = append(names, m[1])
		}
	}
	return names
}

// repoRoot walks up to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (no go.mod found)")
		}
		dir = parent
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
