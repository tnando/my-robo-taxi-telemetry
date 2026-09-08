//go:build contract

// THE "ONE SET" CLAIM, CHECKED AGAINST A REAL POSTGRES (MYR-614).
//
// rest-api.md §5.2.4 tells clients that the drives §7.2 LISTS for a trip
// participant and the drives §7.3/§7.4 let them OPEN are provably one set. Two
// different pieces of code make that true: the list narrows with a SQL
// predicate over `"startTime"::timestamptz` (internal/store/trip_queries.go),
// and the single-drive gate parses the same column in Go
// (telemetry.ParseDriveStartTime).
//
// A claim spanning two parsers is only as good as their agreement, and they did
// not agree: the Go half was strict RFC 3339, which rejects `2026-09-07
// 21:14:00+00` and `2026-09-07` while `::timestamptz` accepts both. A drive
// stored in either shape appeared in a participant's list and answered 404 when
// they opened it — the exact divergence the sentence forbids, reached from the
// parsing side rather than the predicate side.
//
// So this asserts the agreement directly, shape by shape, in both directions:
// what Postgres accepts the gate must accept AT THE SAME INSTANT, and what
// Postgres rejects the gate must reject. A unit test cannot make this claim —
// it would only agree with itself about a shape the database does not take.
package contract_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// TestContract_DriveStartTimeParsesTheSameInGoAndInPostgres.
func TestContract_DriveStartTimeParsesTheSameInGoAndInPostgres(t *testing.T) {
	ctx := context.Background()

	// ZONE-LESS SHAPES RESOLVE AGAINST THE SESSION TimeZone in Postgres and
	// against UTC in Go, so the parity below holds only while the session is
	// UTC. That is the Supabase default and the container default; asserting
	// it here means a change to either is caught as a failure of THIS test
	// rather than as a mystery about a participant's window.
	var timeZone string
	if err := testPool.QueryRow(ctx, "SHOW TimeZone").Scan(&timeZone); err != nil {
		t.Fatalf("SHOW TimeZone: %v", err)
	}
	if timeZone != "UTC" {
		t.Fatalf("session TimeZone = %q, want UTC — the zone-less startTime shapes below "+
			"resolve against it in SQL and against UTC in Go, so the two halves of the "+
			"§5.2.4 \"one set\" claim would disagree by that offset", timeZone)
	}

	t.Run("every shape Postgres accepts, the gate accepts at the same instant", func(t *testing.T) {
		accepted := []string{
			// What the two writers actually produce: Prisma's
			// toISOString() and drive_mapper.go's Format(time.RFC3339).
			"2026-09-07T21:14:00Z",
			"2026-09-07T21:14:00.000Z",
			"2026-09-07T21:14:00.123456Z",
			"2026-09-07T16:14:00-05:00",
			// Offsets Postgres accepts and RFC 3339 does not spell — the
			// MYR-614 divergence.
			"2026-09-07T21:14:00+0000",
			"2026-09-07T21:14:00+00",
			// The space separator: how Postgres itself renders a
			// timestamptz, so what a psql round trip writes back.
			"2026-09-07 21:14:00+00",
			"2026-09-07 16:14:00-05:00",
			"2026-09-07 21:14:00+0000",
			// No offset at all, and truncations.
			"2026-09-07T21:14:00",
			"2026-09-07 21:14:00",
			"2026-09-07T21:14",
			"2026-09-07 21:14",
			"2026-09-07",
		}

		for _, value := range accepted {
			t.Run(value, func(t *testing.T) {
				var inSQL time.Time
				if err := testPool.QueryRow(ctx,
					`SELECT ($1::text)::timestamptz`, value).Scan(&inSQL); err != nil {
					t.Fatalf("Postgres rejected %q, so this row cannot reach a participant's "+
						"list either and the case does not belong here: %v", value, err)
				}

				inGo, ok := telemetry.ParseDriveStartTime(value)
				if !ok {
					t.Fatalf("ParseDriveStartTime(%q) refused a shape `::timestamptz` accepts — "+
						"a drive stored this way LISTS for a trip participant (§7.2) and 404s "+
						"when they open it (§7.3/§7.4)", value)
				}
				if !inGo.Equal(inSQL) {
					t.Errorf("%q reads as %s in Go and %s in Postgres — the window bound is "+
						"evaluated against two different instants", value, inGo.UTC(), inSQL.UTC())
				}
			})
		}
	})

	t.Run("every shape Postgres rejects, the gate rejects too", func(t *testing.T) {
		// A value in one of these shapes cannot be compared to a window by
		// EITHER half: the list's predicate errors on the cast and the gate
		// cannot parse it. Both refuse — which is the fail-closed answer, and
		// on the single-drive surfaces it is reported as the ordinary 404
		// plus an ERROR log (see denyDriveWithUnreadableStartTime).
		rejected := []string{
			"",
			"yesterday afternoon",
			"2026-13-07T21:14:00Z",
			"2026-09-07T21:14:00Z and then some",
		}

		for _, value := range rejected {
			t.Run(value, func(t *testing.T) {
				var inSQL time.Time
				err := testPool.QueryRow(ctx, `SELECT ($1::text)::timestamptz`, value).Scan(&inSQL)
				if err == nil {
					t.Fatalf("Postgres accepted %q as %s, so §7.2 would list a drive stored "+
						"this way while ParseDriveStartTime refuses to open it", value, inSQL)
				}
				if _, ok := telemetry.ParseDriveStartTime(value); ok {
					t.Fatalf("ParseDriveStartTime(%q) parsed a value Postgres will not cast", value)
				}
			})
		}
	})
}
