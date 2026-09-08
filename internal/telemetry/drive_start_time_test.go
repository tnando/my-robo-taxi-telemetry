package telemetry

import (
	"testing"
	"time"
)

// ParseDriveStartTime is the Go half of a claim rest-api.md §5.2.4 makes about
// access: the drives §7.2 lists for a trip participant and the drives §7.3/§7.4
// let them open are ONE SET. §7.2 reads the column with `::timestamptz` in SQL;
// this reads it in Go. These pin the Go half, shape by shape.
//
// The SQL half — that Postgres really does accept exactly these and yield
// exactly these instants — is pinned against a real database in
// tests/contract/drive_start_time_parity_test.go. Neither test is sufficient
// alone: this one would happily agree with itself about a shape Postgres
// rejects.
func TestParseDriveStartTimeAcceptsEveryShapeTheCastAccepts(t *testing.T) {
	want := time.Date(2026, 9, 7, 21, 14, 0, 0, time.UTC)

	cases := []struct {
		name  string
		value string
		want  time.Time
	}{
		// What both writers actually produce.
		{"RFC 3339, Z", "2026-09-07T21:14:00Z", want},
		{"RFC 3339, fractional seconds (Prisma toISOString)", "2026-09-07T21:14:00.000Z", want},
		{"RFC 3339, microseconds", "2026-09-07T21:14:00.123456Z", want.Add(123456 * time.Microsecond)},
		{"RFC 3339, a real offset", "2026-09-07T16:14:00-05:00", want},
		// Offsets Postgres accepts and RFC 3339 does not spell. THESE ARE THE
		// MYR-614 DIVERGENCE: strict RFC 3339 refused them, the cast admits
		// them, so a row in this shape listed and then 404'd on open.
		{"offset without a colon", "2026-09-07T21:14:00+0000", want},
		{"offset hours only", "2026-09-07T21:14:00+00", want},
		// The space separator — how Postgres itself renders a timestamptz.
		{"space separator with an offset", "2026-09-07 21:14:00+00", want},
		{"space separator, offset with a colon", "2026-09-07 16:14:00-05:00", want},
		{"space separator, offset without a colon", "2026-09-07 21:14:00+0000", want},
		// No offset at all — UTC here, session TimeZone there.
		{"no offset, T separator", "2026-09-07T21:14:00", want},
		{"no offset, space separator", "2026-09-07 21:14:00", want},
		{"minute precision", "2026-09-07T21:14", want},
		{"minute precision, space separator", "2026-09-07 21:14", want},
		// Date only — midnight.
		{"date only", "2026-09-07", time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseDriveStartTime(tc.value)
			if !ok {
				t.Fatalf("ParseDriveStartTime(%q) refused a shape `::timestamptz` accepts — "+
					"a drive stored this way lists for a participant and then 404s when they open it", tc.value)
			}
			if !got.Equal(tc.want) {
				t.Errorf("ParseDriveStartTime(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}

// TestUnparseableDriveStartTimeIsAdmittedToNobody.
//
// The window test cannot be evaluated against a start time that will not parse,
// and the fail-closed answer for an unevaluable access check is denial. The
// owner path never reaches this helper. What the CALLER is told about the
// denial is denyDriveWithUnreadableStartTime's business — the ordinary 404,
// because a distinct status would confirm the drive exists.
func TestUnparseableDriveStartTimeIsAdmittedToNobody(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		// THE EXACT MYR-614 SHAPE: the field the adapter never set.
		{"absent — the MYR-614 regression", ""},
		{"not an instant at all", "not an instant"},
		{"prose", "yesterday afternoon"},
		{"a month that does not exist", "2026-13-07T21:14:00Z"},
		{"trailing junk", "2026-09-07T21:14:00Z and then some"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := ParseDriveStartTime(tc.value); ok {
				t.Fatalf("ParseDriveStartTime(%q) parsed", tc.value)
			}
		})
	}
}
