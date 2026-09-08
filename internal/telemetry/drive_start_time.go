package telemetry

import "time"

// ONE PARSER FOR THE DRIVE START INSTANT (MYR-614).
//
// §7.2 admits a trip participant's drives with a SQL predicate —
// `"startTime"::timestamptz BETWEEN win_from AND win_to`
// (internal/store/trip_queries.go) — and §7.3/§7.4 admit a SINGLE drive by
// parsing the same column in Go and folding it over the same windows. The
// contract (rest-api.md §5.2.4) claims those two admit PROVABLY ONE SET.
//
// THAT CLAIM IS ONLY TRUE IF THE TWO READ THE COLUMN THE SAME WAY. Until this
// file, the Go half was a bare `time.Parse(time.RFC3339, …)`, which is STRICTER
// than the cast: Postgres accepts `2026-09-07 21:14:00+00` and `2026-09-07`,
// RFC 3339 accepts neither. A row in either shape appears in a participant's
// list and then refuses them when they open it — the list-vs-detail divergence
// the "one set" sentence exists to forbid, arrived at from the parsing side
// instead of the predicate side.
//
// SO THE LAYOUTS BELOW ARE A DELIBERATE MIRROR OF THE CAST, not a convenience
// list. They cover every ISO-8601 shape `Drive."startTime"` can hold: the
// RFC 3339 both writers actually produce (Prisma's `toISOString()` and
// drive_mapper.go's `Format(time.RFC3339)`), plus the space-separated,
// truncated-offset, offset-less and date-only shapes Postgres would still
// accept from a hand-written INSERT or a backfill.
// tests/contract/drive_start_time_parity_test.go pins the mirror against a
// REAL Postgres, shape by shape, in both directions — that test, not this
// comment, is what keeps the contract's claim honest.
//
// ZONE-LESS SHAPES ARE READ AS UTC, which is what `::timestamptz` does when the
// session `TimeZone` is UTC — the Supabase default, and what the parity test
// asserts its container reports. A deployment that moves the session off UTC
// would shift those shapes here relative to the cast; the offset-bearing shapes
// (everything either writer produces) are unaffected.
var driveStartTimeLayouts = []string{
	// The shape every writer produces. time.Parse accepts a fractional
	// second after `05` even though the layout omits one, so
	// `…:00.123456Z` takes this arm too.
	time.RFC3339,
	// Offsets Postgres accepts and RFC 3339 does not spell.
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05Z07",
	// The space separator — how Postgres itself renders a timestamptz, so
	// the shape a copy-paste round trip through psql produces.
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05Z0700",
	"2006-01-02 15:04:05Z07",
	// No offset at all: UTC here, session TimeZone there.
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	// Minute precision.
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	// Date only — midnight.
	"2006-01-02",
}

// ParseDriveStartTime reads a `Drive` row's stored `startTime` as the instant
// the §7.3/§7.4 trip-window gate tests, accepting the shapes Postgres
// `::timestamptz` accepts for that column (see above).
//
// A drive whose start time will not parse is admitted to NOBODY through a trip:
// the window test cannot be evaluated, and the fail-closed answer for an
// unevaluable access check is denial. What the caller is TOLD about that
// denial is denyDriveWithUnreadableStartTime's business, not this function's.
// The owner path never reaches here.
//
// EXPORTED FOR THE PARITY TEST. It is the only reason — no other package calls
// it — but the test that proves the Go half and the SQL half read one column
// one way has to run against a real database, which puts it in tests/contract.
func ParseDriveStartTime(s string) (time.Time, bool) {
	for _, layout := range driveStartTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
