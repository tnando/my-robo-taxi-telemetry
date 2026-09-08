//go:build contract

// Package contract_test contract conformance harness for the Go REST
// surface (MYR-141 Quality 1 Phase 1).
//
// What this file is: shared helpers for endpoint-level contract tests
// that spin up the real handlers from `internal/telemetry/*`, wired
// against a real Postgres (via testcontainers-go) using the same lean
// repos the production binary mounts. The point is to catch contract
// drift — missing routes (MYR-132's 404 on /drives), data-shape
// mismatches (MYR-139), and schema-breaking changes — on every PR
// against the actual handler code rather than against fixtures alone.
//
// Build tag: `contract`. The default `go test ./...` does NOT execute
// these tests, mirroring the pattern in `internal/store/db_test.go` —
// local dev without Docker still passes. CI runs them in a dedicated
// `Contract conformance` job per `.github/workflows/ci.yml`.
//
// Docker gating: when Docker is unreachable, TestMain logs a skip
// notice and exits 0 without running any tests. This matches the
// behavior in `internal/store/db_test.go` so a workstation without
// Docker is never a hard failure.
//
// Schema validation: bodies are validated against the canonical
// `docs/contracts/schemas/*.schema.json` shapes using
// `github.com/santhosh-tekuri/jsonschema/v6` (already a dependency for
// `fixtures_test.go` — Phase 1 adds no new modules). The task spec
// proposed v5; v6 is the version already vendored, so we reuse it
// rather than introduce a parallel version.
package contract_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
	"github.com/myrobotaxi/telemetry/internal/testutil"
)

// ---------------------------------------------------------------------------
// Shared container state (TestMain)
// ---------------------------------------------------------------------------

// contractTestSecret is the HS256 signing secret the JWT minter and the
// JWTAuthenticator share for all contract tests. Hardcoded because the
// secret is irrelevant outside the test process — what matters is the
// minter and the validator agree.
const contractTestSecret = "contract-test-secret-key" // #nosec G101 -- test-only

// testPool is the shared pgx pool for every contract test. Initialized
// in TestMain and reused across t.Run subtests; each subtest owns its
// data by calling cleanTables before seeding.
var testPool *pgxpool.Pool

// dockerAvailable mirrors internal/store/db_test.go: when false, the
// suite exits 0 instead of running tests against a missing daemon.
var dockerAvailable bool

func TestMain(m *testing.M) {
	if !isDockerRunning() {
		fmt.Fprintln(os.Stderr, "Docker is not available, skipping contract tests")
		os.Exit(0)
	}

	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("contractdb"),
		postgres.WithUsername("testuser"),
		postgres.WithPassword("testpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start postgres container: %v\n", err)
		os.Exit(1)
	}

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get connection string: %v\n", err)
		os.Exit(1)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create pool: %v\n", err)
		os.Exit(1)
	}

	if err := createContractSchema(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create schema: %v\n", err)
		os.Exit(1)
	}

	testPool = pool
	dockerAvailable = true

	code := m.Run()

	pool.Close()
	if err := pgContainer.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to terminate container: %v\n", err)
	}

	os.Exit(code)
}

// isDockerRunning probes the Docker daemon. A 5s ceiling keeps a stalled
// daemon from blocking CI indefinitely.
func isDockerRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info") // #nosec G204 -- hardcoded command, not user input
	return cmd.Run() == nil
}

// createContractSchema provisions the Prisma-owned tables the handlers
// read from. Mirrors the shape in internal/store/db_test.go for
// Vehicle + Drive, plus a minimal User table for the JWTAuthenticator's
// fail-closed existence check (data-lifecycle.md §3.5).
func createContractSchema(ctx context.Context, pool *pgxpool.Pool) error {
	schema := `
	CREATE TYPE "VehicleStatus" AS ENUM (
		'driving', 'parked', 'charging', 'offline', 'in_service'
	);

	-- go_try_timestamptz: the try-cast for the Prisma-owned TEXT "startTime"
	-- column (migration 0070, MYR-608). MIRRORED HERE RATHER THAN MIGRATED IN,
	-- because this harness hand-writes its schema instead of calling
	-- store.RunMigrations — every go_ table below is mirrored the same way and
	-- for the same reason (the Prisma-owned tables are created here by hand, so
	-- the migrations would collide with them).
	--
	-- IT IS NOT OPTIONAL FOR THIS HARNESS. driveStartInstantExpr names this
	-- function, so §7.2 for both roles, §7.30.7 and the trip totals all fail
	-- with "function go_try_timestamptz(text) does not exist" without it — the
	-- contract tests would go red on the first drive list, not on a subtle
	-- answer. Keep the body byte-identical to the migration's.
	CREATE OR REPLACE FUNCTION go_try_timestamptz(value text)
	RETURNS timestamptz
	LANGUAGE plpgsql
	IMMUTABLE
	STRICT
	SET search_path = pg_catalog, pg_temp
	AS $$
	BEGIN
		RETURN value::pg_catalog.timestamptz;
	EXCEPTION
		WHEN others THEN
			RETURN NULL;
	END;
	$$;

	CREATE TABLE "User" (
		"id"   TEXT PRIMARY KEY,
		-- MYR-581: the TOP RUNG of the display-name ladder. Every catalog read
		-- and the snapshot read now resolve the vehicle owner's name through
		-- ownerNameLadderExpr (internal/store/owner_name.go), which selects
		-- u."name" here. Without the column the whole statement fails and every
		-- REST /api/vehicles and /snapshot request answers 500 — which is
		-- exactly how this harness caught the gap.
		"name" TEXT
	);

	-- go_users: the Go-owned identity table (migration 0003, MYR-193).
	-- The JWTAuthenticator's FR-10.1 existence check probes a sub in
	-- EITHER "User" OR go_users (Apple-native users have no Prisma row),
	-- so the harness MUST provision both tables or every authenticated
	-- request fails closed when the go_users EXISTS sub-probe hits a
	-- missing relation. The check only reads "id"; name is the MYR-581
	-- ladder's BOTTOM RUNG.
	CREATE TABLE go_users (
		"id"   TEXT PRIMARY KEY,
		"name" TEXT
	);

	-- go_identity_apple: the apple_sub -> user_id binding (migration 0003,
	-- MYR-193), and the MIDDLE RUNG of the MYR-581 display-name ladder — the
	-- rung that matters most, because it is the only one an Apple-native
	-- account carries a real name on.
	--
	-- Only the columns the ladder reads are provisioned: user_id (the key),
	-- name (the value) and last_login_at (the tie-break — that rung is
	-- ORDER BY last_login_at DESC LIMIT 1, because one user may hold several
	-- bindings). apple_sub comes along as the primary key. Whatever else the
	-- identity module needs is out of scope: this harness exercises the REST
	-- surface, and the REST surface reads names and nothing else from here.
	CREATE TABLE go_identity_apple (
		apple_sub     TEXT PRIMARY KEY,
		user_id       TEXT NOT NULL,
		email         TEXT,
		name          TEXT,
		last_login_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	-- go_profile_name_confirmations: the MYR-583 record that an account has
	-- CONFIRMED its display name (migration 0042). Not a rung of the ladder — it
	-- holds no name — but a PRECONDITION of the ladder mattering: the catalog's
	-- ownerFirstName, the share listing's acceptedByName and the ride-offerability
	-- gate all read it before they will publish or accept a name, because two of
	-- the three rungs are machine-written and a resolvable name is not evidence of
	-- consent. Both columns are provisioned; only row PRESENCE is ever read.
	CREATE TABLE go_profile_name_confirmations (
		user_id      TEXT PRIMARY KEY,
		confirmed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE "Vehicle" (
		"id"               TEXT PRIMARY KEY,
		"userId"           TEXT NOT NULL,
		"teslaVehicleId"   TEXT UNIQUE,
		"vin"              TEXT UNIQUE,
		"name"             TEXT NOT NULL DEFAULT '',
		"model"            TEXT NOT NULL DEFAULT '',
		"year"             INT NOT NULL DEFAULT 0,
		"color"            TEXT NOT NULL DEFAULT '',
		"licensePlate"     TEXT NOT NULL DEFAULT '',
		"chargeLevel"      INT NOT NULL DEFAULT 0,
		"estimatedRange"   INT NOT NULL DEFAULT 0,
		"chargeState"      TEXT,
		"timeToFull"       DOUBLE PRECISION,
		"status"           "VehicleStatus" NOT NULL DEFAULT 'offline',
		"speed"            INT NOT NULL DEFAULT 0,
		"gearPosition"     TEXT,
		"heading"          INT NOT NULL DEFAULT 0,
		"locationName"     TEXT NOT NULL DEFAULT '',
		"locationAddress"  TEXT NOT NULL DEFAULT '',
		-- MYR-447 — encrypted shadows for the geocoded location labels.
		-- Mirrors docs/migrations/myr-447-prisma-label-enc.sql, which has
		-- to be applied as a Prisma migration in react-frontend because
		-- CG-DL-9 forbids a Go migration from naming "Vehicle"/"Drive".
		-- The wide vehicle SELECT reads these INSTEAD OF the plaintext
		-- pair above, so omitting them here is not a lossy fixture — it
		-- is a 42703 on every snapshot and drives read.
		"locationNameEnc"       TEXT,
		"locationAddressEnc"    TEXT,
		"latitude"         DOUBLE PRECISION NOT NULL DEFAULT 0,
		"longitude"        DOUBLE PRECISION NOT NULL DEFAULT 0,
		"latitudeEnc"          TEXT,
		"longitudeEnc"         TEXT,
		"interiorTemp"     INT NOT NULL DEFAULT 0,
		"exteriorTemp"     INT NOT NULL DEFAULT 0,
		"odometerMiles"    INT NOT NULL DEFAULT 0,
		"fsdMilesSinceReset"    DOUBLE PRECISION NOT NULL DEFAULT 0,
		"virtualKeyPaired" BOOLEAN NOT NULL DEFAULT FALSE,
		"destinationName"  TEXT,
		"destinationLatitude"  DOUBLE PRECISION,
		"destinationLongitude" DOUBLE PRECISION,
		"destinationLatitudeEnc"  TEXT,
		"destinationLongitudeEnc" TEXT,
		"originLatitude"       DOUBLE PRECISION,
		"originLongitude"      DOUBLE PRECISION,
		"originLatitudeEnc"    TEXT,
		"originLongitudeEnc"   TEXT,
		"destinationAddress" TEXT,
		-- MYR-447 — encrypted shadows for the destination labels.
		"destinationNameEnc"    TEXT,
		"destinationAddressEnc" TEXT,
		"etaMinutes"       INT,
		"tripDistanceMiles" DOUBLE PRECISION,
		"tripDistanceRemaining" DOUBLE PRECISION,
		"navRouteCoordinates" JSONB,
		"navRouteCoordinatesEnc" TEXT,
		"lastUpdated"      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"createdAt"        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"updatedAt"        TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE "Drive" (
		"id"               TEXT PRIMARY KEY,
		"vehicleId"        TEXT NOT NULL REFERENCES "Vehicle"("id"),
		"date"             TEXT NOT NULL,
		"startTime"        TEXT NOT NULL,
		"endTime"          TEXT NOT NULL DEFAULT '',
		"startLocation"    TEXT NOT NULL DEFAULT '',
		"startAddress"     TEXT NOT NULL DEFAULT '',
		"endLocation"      TEXT NOT NULL DEFAULT '',
		"endAddress"       TEXT NOT NULL DEFAULT '',
		-- MYR-447 — encrypted shadows for the drive's endpoint labels.
		-- Nullable: NULL is the absent sentinel that
		-- queryDriveMissingAddresses' discovery predicate keys on.
		"startLocationEnc" TEXT,
		"startAddressEnc"  TEXT,
		"endLocationEnc"   TEXT,
		"endAddressEnc"    TEXT,
		"distanceMiles"    DOUBLE PRECISION NOT NULL DEFAULT 0,
		"durationMinutes"  INT NOT NULL DEFAULT 0,
		"avgSpeedMph"      DOUBLE PRECISION NOT NULL DEFAULT 0,
		"maxSpeedMph"      DOUBLE PRECISION NOT NULL DEFAULT 0,
		"energyUsedKwh"    DOUBLE PRECISION NOT NULL DEFAULT 0,
		"startChargeLevel" INT NOT NULL DEFAULT 0,
		"endChargeLevel"   INT NOT NULL DEFAULT 0,
		"fsdMiles"         DOUBLE PRECISION NOT NULL DEFAULT 0,
		"fsdPercentage"    DOUBLE PRECISION NOT NULL DEFAULT 0,
		"interventions"    INT NOT NULL DEFAULT 0,
		"routePoints"      JSONB NOT NULL DEFAULT '[]',
		"routePointsEnc"   TEXT,
		"createdAt"        TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	-- go_vehicle_control_state: Go-owned owner-control side table (migration
	-- 0008, MYR-269). VehicleRepo.GetByID LEFT-JOINs it for the /snapshot
	-- read, so the harness MUST provision it or the snapshot + drives
	-- ownership-check reads hit a missing relation and 500. Mirrors 0008.
	CREATE TABLE go_vehicle_control_state (
		"vehicle_id"       TEXT PRIMARY KEY,
		"is_locked"        BOOLEAN,
		"frunk_open"       BOOLEAN,
		"trunk_open"       BOOLEAN,
		"is_climate_on"    BOOLEAN,
		"charge_port_open" BOOLEAN,
		-- MYR-273 cabin settings
		"driver_temp_setting"     INT,
		"passenger_temp_setting"  INT,
		"fan_speed"               INT,
		"seat_heater_left"        INT,
		"seat_heater_right"       INT,
		"seat_heater_rear_left"   INT,
		"seat_heater_rear_center" INT,
		"seat_heater_rear_right"  INT,
		"seat_cooler_left"        INT,
		"seat_cooler_right"       INT,
		"media_volume"            DOUBLE PRECISION,
		-- MYR-279 vehicle-detail read-backs (migration 0011)
		"software_version"        TEXT,
		"trim"                    TEXT,
		-- MYR-274 climate-mode read-backs (migration 0012)
		"hvac_auto_mode"          TEXT,
		"hvac_ac_enabled"         BOOLEAN,
		-- MYR-298 seat-vent + media-playback read-backs (migration 0014)
		"seat_vent_enabled"       BOOLEAN,
		"media_playback_status"   TEXT,
		-- MYR-303 media now-playing + MYR-308 capability (migration 0015)
		"media_now_playing_title"       TEXT,
		"media_now_playing_artist"      TEXT,
		"media_now_playing_album"       TEXT,
		"media_now_playing_station"     TEXT,
		"media_playback_source"         TEXT,
		"media_now_playing_duration_ms" BIGINT,
		"media_now_playing_elapsed_ms"  BIGINT,
		"media_volume_max"              DOUBLE PRECISION,
		"seat_cooling_capable"          BOOLEAN,
		-- MYR-316 service window (migration 0017). The snapshot LEFT JOIN and
		-- the catalog list query both select these, so the harness must carry
		-- them or both endpoints 500 on a missing column.
		"service_etc"             TIMESTAMPTZ,
		"service_expected_end_at" TIMESTAMPTZ,
		-- MYR-320 vehicle details (migration 0018). Same trap as the MYR-316
		-- pair above: GetByID's LEFT JOIN selects both, so omitting them here
		-- 500s the snapshot and drives endpoints on a missing column.
		"trim_label"              TEXT,
		"fsd_version"             TEXT,
		-- MYR-342 owner ride-sharing switch (migration 0021). Same trap as the
		-- two blocks above -- the snapshot LEFT JOIN and BOTH catalog list
		-- queries select it -- plus one of its own: it is the only column here
		-- that is NOT NULL DEFAULT true rather than nullable, and that default
		-- is load-bearing. A nullable copy would let the harness produce a NULL
		-- the real schema cannot, which the readers COALESCE to enabled,
		-- hiding a pause that production would honour.
		"ride_share_enabled"      BOOLEAN NOT NULL DEFAULT true,
		"updated_at"       TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	-- go_ride_requests: the Go-owned ride table (migration 0002,
	-- MYR-173). The MYR-233 catalog list query correlates against it for
	-- the derived hasActiveRide flag, so the harness MUST provision it
	-- or GET /api/vehicles hits a missing relation and 500s. Minimal
	-- shape — the columns the flag's predicate reads plus the NOT NULL
	-- columns any seed must satisfy.
	CREATE TABLE go_ride_requests (
		"id"            TEXT PRIMARY KEY,
		"rider_id"      TEXT NOT NULL,
		"owner_id"      TEXT NOT NULL,
		"vehicle_id"    TEXT NOT NULL,
		"status"        TEXT NOT NULL DEFAULT 'requested',
		"scheduled_for" TIMESTAMPTZ,
		"created_at"    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	-- Mirrors migration 0013 (MYR-266) so the harness exercises the same
	-- partial index the hasActiveRide EXISTS predicate is written against.
	CREATE UNIQUE INDEX uq_go_ride_requests_active_instant_vehicle
		ON go_ride_requests (vehicle_id)
		WHERE scheduled_for IS NULL
		  AND status IN ('accepted', 'enroute', 'arrived');

	-- go_fleet_config_attempts: the Go-owned fleet-config retry schedule
	-- (migrations 0031 + 0036, MYR-448 / MYR-489). MYR-491 LEFT JOINs it into
	-- BOTH the snapshot read and both catalog reads to derive setupState, so
	-- the harness MUST provision it or /snapshot and GET /api/vehicles hit a
	-- missing relation and 500 — exactly how this table announced itself when
	-- the join first landed.
	--
	-- Full shape rather than the reader's four columns. The two paths that
	-- write here in production (RecordFleetConfigAttempt, the link-time seed)
	-- supply attempt_count and next_attempt_at, and a harness missing them
	-- would refuse any seed a future setup-state test wants to plant.
	CREATE TABLE go_fleet_config_attempts (
		"vehicle_id"        TEXT PRIMARY KEY,
		"attempt_count"     INTEGER     NOT NULL DEFAULT 0,
		"last_attempt_at"   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"next_attempt_at"   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"last_outcome"      TEXT        NOT NULL DEFAULT '',
		-- Nullable with NO default, exactly as migration 0036 declares them:
		-- NULL means "never observed", which is materially different from
		-- "observed at row creation" and is what the derivation reads as the
		-- absence of a pairing epoch.
		"signed_command_at" TIMESTAMPTZ,
		"forced_repush_at"  TIMESTAMPTZ
	);

	-- go_vehicle_telemetry_suspensions: the Go-owned owner-inactivity episode
	-- (migration 0044, MYR-592). All three catalog reads LEFT JOIN it to emit
	-- VehicleSummary.telemetrySuspendedAt, so the harness MUST provision it or
	-- GET /api/vehicles hits a missing relation and 500s — the same way
	-- go_fleet_config_attempts above announced itself when its join landed.
	--
	-- Full shape rather than the reader's one column: warned_at is what makes
	-- the day-4 warning fire once per episode, and a harness missing it would
	-- refuse any seed a future suspension test wants to plant.
	CREATE TABLE go_vehicle_telemetry_suspensions (
		"vehicle_id"   TEXT PRIMARY KEY,
		-- Both nullable with NO default, exactly as migration 0044 declares
		-- them: NULL means "has not happened in this episode", which the
		-- catalog reads as "streaming normally".
		"warned_at"    TIMESTAMPTZ,
		"suspended_at" TIMESTAMPTZ
	);

	-- go_user_activity: the Go-owned per-account last-seen row (migration 0043,
	-- MYR-592). NOT read by any endpoint the harness mounts — only the §7.27
	-- sweeper reads it — but the auth path WRITES it on every validated bearer,
	-- and the harness authenticates on every request. Without the table the
	-- stamp's swallow-and-log arm would fire on each one, which is survivable by
	-- design and still exactly the kind of noise that hides a real failure.
	CREATE TABLE go_user_activity (
		"user_id"      TEXT PRIMARY KEY,
		"last_seen_at" TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	-- go_vehicle_driver_access: the Go-owned consent gate for a car linked by
	-- someone Tesla calls a DRIVER of it rather than its owner (migration 0046,
	-- MYR-599). The snapshot read and ALL THREE catalog reads LEFT JOIN it to
	-- emit VehicleSummary/VehicleState.teslaAccessType and the
	-- awaiting_owner_acknowledgment setup state, so the harness MUST provision
	-- it or every vehicle read hits a missing relation and 500s — which is
	-- precisely how this table announced itself, exactly as
	-- go_fleet_config_attempts and go_vehicle_telemetry_suspensions above each
	-- announced themselves when their joins landed.
	--
	-- Full shape rather than the reader's two columns, for the same reason its
	-- neighbours carry theirs: acknowledgment_version and the raw
	-- tesla_access_type are what a future §7.29 conformance test would need to
	-- plant, and a harness missing them would refuse the seed.
	CREATE TABLE go_vehicle_driver_access (
		"vehicle_id"             TEXT PRIMARY KEY,
		"user_id"                TEXT        NOT NULL,
		-- Tesla's access_type VERBATIM, '' included. NOT NULL with no default,
		-- exactly as migration 0046 declares it: a row exists because a listing
		-- said something, so there is no state a NULL could describe.
		"tesla_access_type"      TEXT        NOT NULL,
		-- Both nullable with NO default, exactly as 0046 declares them. NULL on
		-- acknowledged_at IS the shut gate; a default would open every gate the
		-- moment a row was created.
		"acknowledged_at"        TIMESTAMPTZ,
		"acknowledgment_version" TEXT,
		-- NOT NULL, and load-bearing: row presence is read off it through the
		-- catalog LEFT JOIN, so a nullable created_at would make a driver car
		-- indistinguishable from an owner's on every read.
		"created_at"             TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	-- go_trips: the owner-defined WINDOW on one vehicle during which a chosen
	-- subset of its accepted share-holders sees the car live (migration 0047,
	-- MYR-602).
	--
	-- IT IS REACHED FROM THE DRIVES ENDPOINTS THIS HARNESS ALREADY TESTS, which
	-- is what makes it this file's business rather than the trip suite's. All
	-- three viewer reads — vehicle drives, drive detail, drive route — now call
	-- resolveTripDriveAdmission (internal/telemetry/trip_drive_access.go), which
	-- probes go_trip_participants JOIN go_trips for the caller's windows before
	-- deciding what a non-owner may see.
	--
	-- NOTE THE FAILURE MODE, because it is NOT the 500 its neighbours above
	-- announce themselves with, and that is precisely why the table is easy to
	-- forget and expensive to omit. That probe FAILS CLOSED: a missing relation
	-- is logged and returned as "no windows", so the request does not error —
	-- it just denies. A harness lacking these tables therefore keeps every
	-- drives test GREEN while making the participant-admission assertions pass
	-- for the WRONG REASON: they would be observing a denial manufactured by an
	-- absent table rather than by the access rule under test. A green test that
	-- proves nothing is worse than a red one, so the relation has to be here.
	--
	-- The same pair is also the FOURTH UNION leg of queryUserVehicleIDs
	-- (internal/auth/queries.go), the WebSocket handshake's access set. That
	-- path is not mounted by this REST harness and could not run here anyway —
	-- see the go_vehicle_shares note below — but the tables are shaped for it.
	--
	-- Full shape rather than the two columns leg 4 reads, for the reason its
	-- neighbours carry theirs: the window pair, the early-end stamp and the
	-- sweeper's two idempotency stamps are what a §7.30 conformance test would
	-- need to plant to move a trip through its lifecycle, and a harness missing
	-- them would refuse the seed. The two CHECKs come along for the same reason
	-- migration 0047 put them in the schema rather than the handler: a test that
	-- can seed a zero-length or decade-long window is a test that can assert
	-- behaviour the server will never produce.
	CREATE TABLE go_trips (
		"id"                  TEXT        PRIMARY KEY,
		-- Opaque Prisma cuids, NO foreign key, exactly as 0047 declares them
		-- (CG-DL-9 forbids naming a Prisma table here).
		"vehicle_id"          TEXT        NOT NULL,
		"owner_user_id"       TEXT        NOT NULL,
		-- P1 user content, AES-256-GCM sealed. NOT NULL: the create endpoint
		-- requires 1..60 characters after trimming, so there is no nameless
		-- trip and therefore no absent sentinel to express.
		"name_enc"            TEXT        NOT NULL,
		-- starts_at MAY be in the past: that is how the legs of a road trip
		-- already driven join the trip retroactively.
		"starts_at"           TIMESTAMPTZ NOT NULL,
		"ends_at"             TIMESTAMPTZ NOT NULL,
		-- Set when the OWNER ENDS THE TRIP EARLY. Readers compute the effective
		-- end as LEAST(ends_at, ended_at) rather than writing back over ends_at.
		"ended_at"            TIMESTAMPTZ,
		-- The sweeper's at-most-once stamps. Nullable instants, not booleans.
		"started_notified_at" TIMESTAMPTZ,
		"ended_notified_at"   TIMESTAMPTZ,
		"created_at"          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"updated_at"          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		CONSTRAINT go_trips_window_ordered CHECK ("ends_at" > "starts_at"),
		CONSTRAINT go_trips_window_capped
			CHECK ("ends_at" <= "starts_at" + INTERVAL '30 days')
	);

	-- go_trip_participants: who is in the window, and when they left. The other
	-- half of every query described above, and therefore the other half of the
	-- silent deny that an absent relation produces.
	--
	-- KNOWN REMAINING GAP, recorded here because it bounds what these two tables
	-- buy: the trip-admission queries also JOIN go_vehicle_shares, and this
	-- harness has never provisioned it (nor go_ride_members). So the probe still
	-- fails closed here, and a participant-admission test cannot yet be written
	-- against this schema — it would need both. The trips tables are necessary
	-- and not sufficient; adding the shares table is its own change, with its
	-- own reasons, and is deliberately not smuggled in here.
	--
	-- Full shape: share_id is not read by the access query — which deliberately
	-- re-joins go_vehicle_shares on (vehicle, user) rather than trusting this
	-- column — but it IS the wire contract's participantId, so a roster
	-- conformance test cannot round-trip without it. left_at is the tombstone
	-- the access query filters on, so it is load-bearing here and not decoration.
	CREATE TABLE go_trip_participants (
		"trip_id"  TEXT NOT NULL REFERENCES go_trips ("id") ON DELETE CASCADE,
		"user_id"  TEXT NOT NULL,
		"share_id" TEXT NOT NULL,
		"added_at" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		-- Tombstone rather than DELETE: re-adding is an UPDATE of one row.
		"left_at"  TIMESTAMPTZ,
		PRIMARY KEY ("trip_id", "user_id")
	);

	-- go_trip_activity_tokens: ActivityKit PUSH-TO-START tokens, one per
	-- (trip, user). Not on the access path, so it does not carry leg 4's
	-- blast radius — it is provisioned because the trip endpoints the harness
	-- mounts write it, and a §7.30 registration test has nowhere to land
	-- without it. It is also a CASCADE target of go_trips: creating go_trips
	-- without it would leave the cascade half-declared and a trip deletion test
	-- asserting against a table that does not exist.
	CREATE TABLE go_trip_activity_tokens (
		"trip_id"             TEXT        NOT NULL REFERENCES go_trips ("id") ON DELETE CASCADE,
		-- The OWNER may hold a row here too — the owner is on the per-leg
		-- Activity by explicit product decision — so this is deliberately not
		-- constrained to participants.
		"user_id"             TEXT        NOT NULL,
		-- P1 CAPABILITY. Never logged beyond an 8-character prefix, never
		-- echoed in a response, never in an error envelope.
		"push_to_start_token" TEXT        NOT NULL,
		"sandbox"             BOOLEAN     NOT NULL DEFAULT FALSE,
		"created_at"          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"updated_at"          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		-- UPSERT target: ActivityKit rotates the token, so a re-registration
		-- REPLACES the value in place rather than accumulating rows.
		PRIMARY KEY ("trip_id", "user_id")
	);

	-- go_trip_legs: one row per driving leg the car takes inside a window. Like
	-- the tokens table it is off the access path, and it is here because the
	-- trip detail response renders the current leg — so a §7.30 read of a trip
	-- with a live leg has nothing to project without it — and because it is the
	-- second CASCADE target of go_trips.
	--
	-- Full shape including the four delivery stamps. They are four and not two
	-- on purpose: the alert pushes and the Live Activity fan-out are separate
	-- deliveries with separate failure modes, and a harness that collapsed them
	-- could not seed the state where one succeeded and the other must retry.
	CREATE TABLE go_trip_legs (
		"id"                   TEXT        PRIMARY KEY,
		"trip_id"              TEXT        NOT NULL REFERENCES go_trips ("id") ON DELETE CASCADE,
		-- Denormalised from the trip so the detector's hot path needs no join.
		-- Opaque cuid, no FK (CG-DL-9).
		"vehicle_id"           TEXT        NOT NULL,
		-- P1 place name, AES-256-GCM sealed. NOT NULL: a leg is DEFINED as
		-- driving WITH a destination, so there is no destinationless leg.
		"destination_name_enc" TEXT        NOT NULL,
		"started_at"           TIMESTAMPTZ NOT NULL,
		-- NULL while the leg is underway.
		"ended_at"             TIMESTAMPTZ,
		-- TRUE only on real ARRIVAL EVIDENCE (80 m / 20 s dwell). Load-bearing:
		-- it decides trip_leg_arrived and the final content-state's status.
		"arrived"              BOOLEAN     NOT NULL DEFAULT FALSE,
		"started_notified_at"  TIMESTAMPTZ,
		"arrived_notified_at"  TIMESTAMPTZ,
		"activity_started_at"  TIMESTAMPTZ,
		"activity_ended_at"    TIMESTAMPTZ,
		"created_at"           TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	-- go_live_activities: the Live Activity registry, IN ITS POST-0047 SHAPE.
	--
	-- Provisioned here because migration 0047's riskiest statement lives on this
	-- table and had no conformance coverage at all: it DROPS ride_request_id's
	-- NOT NULL so a leg can be the anchor instead, and compensates in the same
	-- migration with go_live_activities_one_anchor. A harness that omitted the
	-- table would let every trip test pass while saying nothing about whether
	-- the compensation is installed — and a dropped NOT NULL with an absent
	-- CHECK is a row anchored to NOTHING, addressed to a phone, that no cascade
	-- can ever reach.
	--
	-- THE CHECK IS THE POINT, and it is written here exactly as 0047 writes it
	-- (<> over the two NULL tests, which is XOR for booleans) rather than as a
	-- looser paraphrase: EXACTLY ONE anchor, never both and never neither. It is
	-- STRICTER than the constraint it replaced, which is what makes the drop
	-- safe — no state the old schema forbade is permitted by the new one.
	--
	-- The ride FK is deliberately NOT declared. go_ride_requests is provisioned
	-- above, but 0047's own leg reference (REFERENCES go_trip_legs) is what
	-- the trips path depends on, and keeping the ride side FK-free here matches
	-- what this harness does everywhere else: model the columns and the
	-- constraints a conformance assertion can observe, not the whole schema.
	--
	-- The two UNIQUEs are both here because they answer different questions and
	-- one of them is PARTIAL: the ride pair is a table constraint (Postgres
	-- treats NULLs as distinct, so trip rows never collide through it), while
	-- the leg pair must be a partial index so the ride rows are EXCLUDED rather
	-- than merely tolerated.
	CREATE TABLE go_live_activities (
		"id"                  TEXT        PRIMARY KEY,
		-- NULLABLE since 0047. Was NOT NULL; see the CHECK below.
		"ride_request_id"     TEXT,
		-- The second anchor (0047). Cascades with the leg.
		"trip_leg_id"         TEXT        REFERENCES go_trip_legs ("id") ON DELETE CASCADE,
		"user_id"             TEXT        NOT NULL,
		-- P1 CAPABILITY: addresses ONE RUNNING CARD. Distinct from
		-- go_trip_activity_tokens.push_to_start_token, which addresses the APP.
		"activity_push_token" TEXT        NOT NULL,
		"sandbox"             BOOLEAN     NOT NULL DEFAULT FALSE,
		"created_at"          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"updated_at"          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"ended_at"            TIMESTAMPTZ,
		CONSTRAINT go_live_activities_ride_user_key UNIQUE ("ride_request_id", "user_id"),
		CONSTRAINT go_live_activities_one_anchor
			CHECK (("ride_request_id" IS NOT NULL) <> ("trip_leg_id" IS NOT NULL))
	);

	CREATE UNIQUE INDEX idx_go_live_activities_leg_user
		ON go_live_activities ("trip_leg_id", "user_id")
		WHERE "trip_leg_id" IS NOT NULL;

	-- go_vehicle_shares: the STANDING GRANT (migration 0020, plus 0024's
	-- allow_rides / suspended_at).
	--
	-- Provisioned because MYR-602 made it unavoidable rather than merely
	-- useful. auth.queryUserVehicleIDs is FOUR UNION legs, and TWO of them read
	-- this table — the plain share and, through a join, the trip participation
	-- — so without it the access-set query cannot run in this harness at all
	-- and a participant-admission test could not be written. The trips tables
	-- alone were necessary and not sufficient, which the harness comment used
	-- to say instead of fixing.
	--
	-- THE TWO PREDICATES ARE WHY IT IS THE FULL SHAPE and not two columns:
	-- "status = 'accepted' AND suspended_at IS NULL" is the access predicate
	-- carried CHARACTER-FOR-CHARACTER by six statements across three packages
	-- (auth.queryUserVehicleIDs, auth.queryActiveTripParticipation,
	-- store.queryTripAudience, store.queryTripActivityTokens and the two
	-- catalog merges). A harness that could not plant a suspended row could not
	-- observe any of them, and "a suspended grantee is indistinguishable from
	-- no grantee" is the property the whole trips access model rests on.
	--
	-- The CHECK constraints are 0020's verbatim: a conformance harness that
	-- accepted a status the migration forbids would let a test plant a state
	-- production cannot reach and then assert about it.
	CREATE TABLE go_vehicle_shares (
		"id"                  TEXT        PRIMARY KEY,
		"vehicle_id"          TEXT        NOT NULL,
		"owner_user_id"       TEXT        NOT NULL,
		-- P1 user content, like the trip name: never logged, never in an error.
		"label"               TEXT        NOT NULL,
		"permission"          TEXT        NOT NULL
			CONSTRAINT go_vehicle_shares_permission_check
			CHECK ("permission" IN ('live', 'live_history', 'rides')),
		-- P1 CAPABILITY: whoever holds the code can redeem the grant.
		"code"                TEXT        NOT NULL,
		"status"              TEXT        NOT NULL DEFAULT 'pending'
			CONSTRAINT go_vehicle_shares_status_check
			CHECK ("status" IN ('pending', 'accepted', 'revoked')),
		"created_at"          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"expires_at"          TIMESTAMPTZ NOT NULL,
		"accepted_at"         TIMESTAMPTZ,
		"accepted_by_user_id" TEXT,
		"revoked_at"          TIMESTAMPTZ,
		-- 0024. allow_rides is what a trip participant KEEPS for the length of
		-- the window (ResolveVehicleAccess returns their own grant alongside
		-- the elevated role); suspended_at is the half of the access predicate
		-- a status check alone would miss.
		"allow_rides"         BOOLEAN     NOT NULL DEFAULT FALSE,
		"suspended_at"        TIMESTAMPTZ
	);

	-- The access-set index, 0020's shape verbatim: (accepted_by_user_id,
	-- vehicle_id) leading on the PERSON, because "which vehicles has this
	-- person been granted?" is what runs on every handshake.
	CREATE UNIQUE INDEX uq_go_vehicle_shares_accepted_grant
		ON go_vehicle_shares ("accepted_by_user_id", "vehicle_id")
		WHERE "status" = 'accepted';

	-- go_ride_members: the group-ride roster (migration 0040, MYR-540).
	--
	-- The THIRD UNION leg of auth.queryUserVehicleIDs, and provisioned for the
	-- same reason as the shares table beside it: the access-set query names all
	-- four relations in one statement, so a harness missing any one of them
	-- cannot run it, and the ride_member role — which sees live location, and
	-- is therefore one of the two roles the narrowing had to keep — could not
	-- be exercised against this schema at all.
	--
	-- The ride FK is declared because go_ride_requests is provisioned above and
	-- 0040 declares it: a member row outliving its ride would be a person
	-- holding live location on a car through a ride that no longer exists.
	CREATE TABLE go_ride_members (
		"ride_id"   TEXT        NOT NULL REFERENCES go_ride_requests ("id") ON DELETE CASCADE,
		"user_id"   TEXT        NOT NULL,
		"joined_at" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		CONSTRAINT pk_go_ride_members PRIMARY KEY ("ride_id", "user_id")
	);

	-- The user-leading index the access-set leg needs; the primary key leads on
	-- the ride and is useless for it.
	CREATE INDEX idx_go_ride_members_user ON go_ride_members ("user_id");`
	if _, err := pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Test server: real handlers, real repos, slim mux
// ---------------------------------------------------------------------------

// seedHelpers exposes the seed primitives every test needs to plant
// rows into the test DB. Returned by setupTestServer so a test holds a
// pointer to both the server and the seed surface in one value.
//
// `enc` is the SAME Encryptor the repos under test were built with
// (MYR-447). Every geocoded label a seed plants is sealed with it —
// the handlers read the labels from the `*Enc` columns only, so a seed
// holding a different key (or none) would surface as "this car has no
// address" rather than as a failure anyone could diagnose.
type seedHelpers struct {
	pool *pgxpool.Pool
	enc  cryptox.Encryptor
}

// setupTestServer wires the three REST handlers under test against a
// real Postgres and a real JWTAuthenticator. The mux is intentionally
// lean — only the three routes Phase 1 covers are mounted; cmd/
// composition concerns (TLS, metrics, fleet config, debug stream) are
// out of scope. The signature mirrors the structure proposed in the
// task: `(*httptest.Server, *seedHelpers)`.
//
// The httptest.Server and the test pool are torn down via t.Cleanup
// so callers don't need defer.
func setupTestServer(t *testing.T) (*httptest.Server, *seedHelpers) {
	t.Helper()
	if !dockerAvailable {
		t.Skip("Docker not available; skipping contract test")
	}

	cleanTables(t, testPool)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	// MYR-447: the encryption-aware constructors, not the legacy keyless
	// ones. A keyless repo reads NO location at all — neither coordinates
	// (MYR-433) nor labels — so the keyless harness would have asserted
	// against a wire payload production never emits: every address blank.
	// The seeds below share this exact Encryptor.
	enc := newContractEncryptor(t)
	vehicleRepo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)
	driveRepo := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)

	// Issuer / audience left empty: the test minter omits them too, so
	// the JWT validator skips those checks. This mirrors the dev-mode
	// shape — sufficient for contract conformance (we care about
	// route + body shape, not claim-policy enforcement).
	authenticator := auth.NewJWTAuthenticator(contractTestSecret, "", "", testPool)

	listAdapter := &contractVehicleLister{repo: vehicleRepo}
	snapshotAdapter := &contractVehicleSnapshotReader{repo: vehicleRepo}
	driveAdapter := &contractDriveLister{repo: driveRepo}

	listHandler := telemetry.NewVehiclesListHandler(authenticator, listAdapter, logger)
	// MYR-602: the SHARE READER is wired now that go_vehicle_shares is
	// provisioned. Without it the handler is owner-only and answers 403 to
	// every non-owner, which made the whole non-owner half of the mask —
	// viewer, ride_member and trip_participant — unreachable from this harness.
	// The role resolver beside it is what then decides WHICH of the three the
	// caller is, and the two together are the composition the trips access
	// model actually is.
	shareRepo := store.NewVehicleShareRepo(testPool, logger)
	shareReader := &contractShareReader{repo: shareRepo}
	snapshotHandler := telemetry.NewVehicleSnapshotHandler(
		authenticator, snapshotAdapter, logger,
		telemetry.WithSnapshotRoleResolver(authenticator),
		telemetry.WithSnapshotShareReader(shareReader),
	)
	drivesHandler := telemetry.NewVehicleDrivesHandler(
		authenticator, snapshotAdapter, driveAdapter, logger,
		telemetry.WithDrivesRoleResolver(authenticator),
		// MYR-602's window gate, wired for the reason the share reader above
		// is: without it §7.2 is owner-only and the entire non-owner half of
		// this surface — the narrowed page, and MYR-608's role-scoped
		// `tripId` — is unreachable from this harness.
		telemetry.WithDrivesTripAdmitter(&contractTripDriveAdmitter{
			repo: store.NewTripRepo(testPool, store.NoopMetrics{}, enc, logger),
		}),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/vehicles", listHandler.ServeHTTP)
	mux.HandleFunc("GET /api/vehicles/{vehicleId}/snapshot", snapshotHandler.ServeHTTP)
	mux.HandleFunc("GET /api/vehicles/{vehicleId}/drives", drivesHandler.ServeHTTP)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv, &seedHelpers{pool: testPool, enc: enc}
}

// newContractEncryptor builds an AES-256-GCM Encryptor over a randomly
// generated per-test key, going through the production loader
// (LoadKeySetFromEnv, single-key shorthand) rather than hand-rolling a
// KeySet. Mirrors newTestEncryptor in internal/store/account_repo_test.go
// — the two suites are in different packages, so the helper cannot be
// shared, but the sealing itself is (testutil.SealLabel).
//
// A fresh key per test is safe because the seeds and the reads happen
// inside the same test: nothing sealed here outlives cleanTables.
func newContractEncryptor(t *testing.T) cryptox.Encryptor {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(raw))
	ks, err := cryptox.LoadKeySetFromEnv()
	if err != nil {
		t.Fatalf("LoadKeySetFromEnv: %v", err)
	}
	enc, err := cryptox.NewEncryptor(ks)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

// ---------------------------------------------------------------------------
// Real-repo adapters (mirror cmd/telemetry-server/adapters.go but live
// in the test package so we don't introduce a cyclic dep through cmd/)
// ---------------------------------------------------------------------------

// contractShareReader binds the standing-grant read onto the handler's seam,
// mirroring cmd/telemetry-server's shareReaderAdapter exactly — including the
// property that makes a SUSPENDED grant indistinguishable from no grant: the
// store's statement excludes suspended rows, so there is no paused grant for
// this adapter to hand a gate, and no gate has to name suspension.
type contractShareReader struct{ repo *store.VehicleShareRepo }

func (a *contractShareReader) ShareGrantFor(ctx context.Context, userID, vehicleID string) (auth.ShareGrant, error) {
	allowRides, err := a.repo.ShareGrantFor(ctx, userID, vehicleID)
	if err != nil {
		return auth.ShareGrant{}, err
	}
	return auth.ShareGrant{AllowRides: allowRides}, nil
}

type contractVehicleLister struct{ repo *store.VehicleRepo }

func (a *contractVehicleLister) ListByUser(ctx context.Context, userID string) ([]telemetry.VehicleCatalogRow, error) {
	rows, err := a.repo.ListSummariesByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("contractVehicleLister.ListByUser: %w", err)
	}
	out := make([]telemetry.VehicleCatalogRow, 0, len(rows))
	for i := range rows {
		v := &rows[i]
		out = append(out, telemetry.VehicleCatalogRow{
			ID:             v.ID,
			VIN:            v.VIN,
			Name:           v.Name,
			Model:          v.Model,
			Year:           v.Year,
			Color:          v.Color,
			Status:         string(v.Status),
			ChargeLevel:    v.ChargeLevel,
			EstimatedRange: v.EstimatedRange,
			LastUpdated:    v.LastUpdated,
			// MYR-342: copied explicitly, and it is the one field in this
			// projection whose OMISSION is not merely lossy — the Go zero value
			// false reads as PAUSED, so leaving it out would make the harness
			// assert against a withdrawn vehicle the store never produced.
			RideShareEnabled: v.RideShareEnabled,
			// MYR-581: the owner's first name, already resolved AND already
			// reduced by the store's ladder. Copied explicitly for the same
			// reason as the line above — an omission here is not visible as a
			// compile error, it just makes every owner look nameless, which on
			// this field means "and their car refuses ride requests".
			//
			// This adapter is a HAND-WRITTEN MIRROR of
			// cmd/telemetry-server/adapters.go, so a field added to the catalog
			// and not added here is silently dropped for the whole contract
			// suite — the harness would then validate a payload production never
			// emits. That is a known hazard of the mirror (several other fields
			// are already missing from it and their tests happen not to notice);
			// it is called out here rather than fixed wholesale, because a field
			// this one's tests DO depend on must not join them.
			OwnerFirstName:       v.OwnerFirstName,
			TelemetrySuspendedAt: v.TelemetrySuspendedAt,
			// MYR-491: raw schedule, mirroring cmd/telemetry-server's
			// setupScheduleRow. The handler derives the wire state from it
			// together with Status and LastUpdated above, so all three must
			// travel together or the harness exercises a projection production
			// does not have.
			SetupSchedule: contractSetupSchedule(v.SetupSchedule),
		})
	}
	return out, nil
}

// contractSetupSchedule mirrors cmd/telemetry-server's setupScheduleRow: the
// store and telemetry copies of this shape are deliberately separate types (the
// telemetry package must not import the store), so every adapter — production
// and harness alike — needs the same five-field copy.
func contractSetupSchedule(s store.SetupSchedule) telemetry.VehicleSetupSchedule {
	return telemetry.VehicleSetupSchedule{
		Present:         s.Present,
		LastOutcome:     s.LastOutcome,
		LastAttemptAt:   s.LastAttemptAt,
		SignedCommandAt: s.SignedCommandAt,
		ForcedRepushAt:  s.ForcedRepushAt,
	}
}

type contractVehicleSnapshotReader struct{ repo *store.VehicleRepo }

func (a *contractVehicleSnapshotReader) GetByID(ctx context.Context, vehicleID string) (telemetry.VehicleSnapshotRow, error) {
	v, err := a.repo.GetByID(ctx, vehicleID)
	if err != nil {
		return telemetry.VehicleSnapshotRow{}, fmt.Errorf("contractVehicleSnapshotReader.GetByID: %w", err)
	}
	return telemetry.VehicleSnapshotRow{
		ID:                   v.ID,
		UserID:               v.UserID,
		VIN:                  v.VIN,
		Name:                 v.Name,
		Model:                v.Model,
		Year:                 v.Year,
		Color:                v.Color,
		Status:               string(v.Status),
		ChargeLevel:          v.ChargeLevel,
		EstimatedRange:       v.EstimatedRange,
		ChargeState:          v.ChargeState,
		TimeToFull:           v.TimeToFull,
		Speed:                v.Speed,
		GearPosition:         v.GearPosition,
		Heading:              v.Heading,
		Latitude:             v.Latitude,
		Longitude:            v.Longitude,
		LocationName:         v.LocationName,
		LocationAddress:      v.LocationAddress,
		InteriorTemp:         v.InteriorTemp,
		ExteriorTemp:         v.ExteriorTemp,
		OdometerMiles:        v.OdometerMiles,
		FsdMilesSinceReset:   v.FsdMilesSinceReset,
		DestinationName:      v.DestinationName,
		DestinationAddress:   v.DestinationAddress,
		DestinationLatitude:  v.DestinationLatitude,
		DestinationLongitude: v.DestinationLongitude,
		OriginLatitude:       v.OriginLatitude,
		OriginLongitude:      v.OriginLongitude,
		EtaMinutes:           v.EtaMinutes,
		TripDistRemaining:    v.TripDistRemaining,
		NavRouteCoordinates:  v.NavRouteCoordinates,
		LastUpdated:          v.LastUpdated,
		// MYR-342: same trap as the catalog projection above — an omitted copy
		// is not a missing field on the wire, it is a `false`, i.e. PAUSED.
		RideShareEnabled: v.RideShareEnabled,
		// MYR-491: raw schedule; the handler derives setupState from it.
		SetupSchedule: contractSetupSchedule(v.SetupSchedule),
	}, nil
}

type contractDriveLister struct{ repo *store.DriveRepo }

func (a *contractDriveLister) ListByVehicleID(ctx context.Context, vehicleID, viewerUserID string, cursor telemetry.DriveListCursor, limit int) (telemetry.DriveListPage, error) {
	page, err := a.repo.ListByVehicleID(ctx, vehicleID, viewerUserID, store.DriveListCursor{
		StartTime: cursor.StartTime,
		ID:        cursor.ID,
	}, limit)
	if err != nil {
		return telemetry.DriveListPage{}, fmt.Errorf("contractDriveLister.ListByVehicleID: %w", err)
	}
	items := make([]telemetry.DriveListItem, 0, len(page.Items))
	for i := range page.Items {
		d := &page.Items[i]
		items = append(items, telemetry.DriveListItem{
			ID:               d.ID,
			VehicleID:        d.VehicleID,
			StartTime:        d.StartTime,
			EndTime:          d.EndTime,
			Date:             d.Date,
			StartLocation:    d.StartLocation,
			StartAddress:     d.StartAddress,
			EndLocation:      d.EndLocation,
			EndAddress:       d.EndAddress,
			DistanceMiles:    d.DistanceMiles,
			DurationMinutes:  d.DurationMinutes,
			AvgSpeedMph:      d.AvgSpeedMph,
			MaxSpeedMph:      d.MaxSpeedMph,
			StartChargeLevel: d.StartChargeLevel,
			EndChargeLevel:   d.EndChargeLevel,
			// MYR-152's two FSD stats. Dropped by this harness since they
			// landed, so every drive it served carried 0.0 for both while the
			// store had real values — a silent wrongness that nothing catches
			// because `validateDrivesList` does not assert them either (the
			// canonical `drives.json` fixture predates MYR-152 as well). Copied
			// now so a future tightening of either finds the truth here.
			FsdMiles:      d.FsdMiles,
			FsdPercentage: d.FsdPercentage,
			CreatedAt:     d.CreatedAt,
			// MYR-608. Already role-scoped by the statement that produced it.
			TripID: d.TripID,
		})
	}
	return telemetry.DriveListPage{Items: items, HasMore: page.HasMore}, nil
}

// contractTripDriveAdmitter is the MYR-602 window gate over the real TripRepo.
//
// WIRED SO THE NON-OWNER HALF OF §7.2 IS REACHABLE FROM THIS HARNESS AT ALL.
// Without it the drives handler is owner-only, exactly as a deployment that
// forgot to wire trips would be — the fail-closed default — and a
// `trip_participant` reading a car's drives could not be exercised end to end.
// It is the same two-method shape cmd/telemetry-server wires; the duplication
// is the price of the package boundary, and the methods are thin enough that
// there is nothing here to get wrong independently.
type contractTripDriveAdmitter struct{ repo *store.TripRepo }

func (a *contractTripDriveAdmitter) TripDriveWindows(
	ctx context.Context, userID, vehicleID string,
) ([]telemetry.TripDriveWindow, error) {
	windows, err := a.repo.TripDriveWindows(ctx, userID, vehicleID)
	if err != nil {
		return nil, fmt.Errorf("contractTripDriveAdmitter.TripDriveWindows: %w", err)
	}
	out := make([]telemetry.TripDriveWindow, 0, len(windows))
	for _, w := range windows {
		out = append(out, telemetry.TripDriveWindow{From: w.From, To: w.To})
	}
	return out, nil
}

func (a *contractTripDriveAdmitter) VehicleDrivesInTripWindows(
	ctx context.Context, userID, vehicleID string, cursor telemetry.DriveListCursor, limit int,
) (telemetry.DriveListPage, error) {
	page, err := a.repo.VehicleDrivesInTripWindows(ctx, userID, vehicleID, store.DriveListCursor{
		StartTime: cursor.StartTime,
		ID:        cursor.ID,
	}, limit)
	if err != nil {
		return telemetry.DriveListPage{}, fmt.Errorf("contractTripDriveAdmitter.VehicleDrivesInTripWindows: %w", err)
	}
	items := make([]telemetry.DriveListItem, 0, len(page.Items))
	for i := range page.Items {
		d := &page.Items[i]
		items = append(items, telemetry.DriveListItem{
			ID:               d.ID,
			VehicleID:        d.VehicleID,
			StartTime:        d.StartTime,
			EndTime:          d.EndTime,
			Date:             d.Date,
			StartLocation:    d.StartLocation,
			StartAddress:     d.StartAddress,
			EndLocation:      d.EndLocation,
			EndAddress:       d.EndAddress,
			DistanceMiles:    d.DistanceMiles,
			DurationMinutes:  d.DurationMinutes,
			AvgSpeedMph:      d.AvgSpeedMph,
			MaxSpeedMph:      d.MaxSpeedMph,
			StartChargeLevel: d.StartChargeLevel,
			EndChargeLevel:   d.EndChargeLevel,
			FsdMiles:         d.FsdMiles,
			FsdPercentage:    d.FsdPercentage,
			CreatedAt:        d.CreatedAt,
			TripID:           d.TripID,
		})
	}
	return telemetry.DriveListPage{Items: items, HasMore: page.HasMore}, nil
}

// ---------------------------------------------------------------------------
// Token minting
// ---------------------------------------------------------------------------

// mintToken signs an HS256 JWT using the shared contract-test secret.
// The `scopes` argument is reserved for future per-scope checks
// (rest-api.md §3.x); v1 carries it as a claim but the validator
// ignores it. Kept in the signature so that callers under §7.0 / §7.1
// / §7.2 don't need refactoring when scope-based gating lands.
//
//nolint:unparam // `scopes` is the contract-test harness public API per MYR-141.
func mintToken(t *testing.T, userID string, scopes []string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": userID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	}
	if len(scopes) > 0 {
		claims["scopes"] = scopes
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(contractTestSecret))
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	return signed
}

// mintExpiredToken returns a JWT signed with the right secret but with
// `exp` in the past. Used by the 401 expired-token case.
func mintExpiredToken(t *testing.T, userID string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": userID,
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(contractTestSecret))
	if err != nil {
		t.Fatalf("mintExpiredToken: %v", err)
	}
	return signed
}

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

// cleanTables truncates the contract test tables. Called by
// setupTestServer for every test so subtests can seed freely without
// worrying about leaks from earlier tests. Order matters: Drive has a
// FK to Vehicle.
func cleanTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM "Drive"`); err != nil {
		t.Fatalf("clean Drive: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM "Vehicle"`); err != nil {
		t.Fatalf("clean Vehicle: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM "User"`); err != nil {
		t.Fatalf("clean User: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM go_users`); err != nil {
		t.Fatalf("clean go_users: %v", err)
	}
	// MYR-581: the ladder's middle rung. Left behind, a stale binding would
	// OUT-RANK the next test's go_users name and resolve a name from a previous
	// test's owner — a cross-test leak that would look like a resolution bug.
	if _, err := pool.Exec(ctx, `DELETE FROM go_identity_apple`); err != nil {
		t.Fatalf("clean go_identity_apple: %v", err)
	}
	// MYR-583: the confirmation records. Left behind, a previous test's
	// confirmation would make the NEXT test's owner look confirmed — which shows
	// up as a name appearing on a row that should read null, or as a car being
	// offerable when the arm meant to prove it is not.
	if _, err := pool.Exec(ctx, `DELETE FROM go_profile_name_confirmations`); err != nil {
		t.Fatalf("clean go_profile_name_confirmations: %v", err)
	}
	// MYR-602. BOTH must go, and go BEFORE the grants they point at would have
	// been removed by the Vehicle delete above: a surviving trip window on a
	// re-used vehicle id would silently ELEVATE the next test's share-holder to
	// trip_participant, which shows up as a viewer reading a real coordinate —
	// a leak that looks exactly like the bug the narrowing exists to prevent.
	// go_trip_participants cascades from go_trips, so one statement covers both.
	if _, err := pool.Exec(ctx, `DELETE FROM go_trips`); err != nil {
		t.Fatalf("clean go_trips: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM go_vehicle_shares`); err != nil {
		t.Fatalf("clean go_vehicle_shares: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM go_ride_members`); err != nil {
		t.Fatalf("clean go_ride_members: %v", err)
	}
}

// seedUser inserts a minimal User row so the JWTAuthenticator's FR-10.1
// fail-closed existence check (data-lifecycle.md §3.5) accepts the
// caller's token. A test that mints a token for userID MUST seed that
// user first; ValidateToken rejects unknown subjects.
func (h *seedHelpers) seedUser(ctx context.Context, t *testing.T, userID string) {
	t.Helper()
	if _, err := h.pool.Exec(ctx, `INSERT INTO "User" ("id") VALUES ($1)`, userID); err != nil {
		t.Fatalf("seedUser(%s): %v", userID, err)
	}
}

// The three MYR-581 display-name rungs, one seeder each, deliberately SEPARATE
// from seedUser.
//
// seedUser plants a NAMELESS account, and it must stay that way: that is the
// ordinary state of an Apple-native account, it is what every other contract test
// in this package means by "an owner", and it is the state the nameless-owner gate
// refuses. A test that wants a name says so.
//
// Each helper writes exactly ONE rung so a test can prove which rung the ladder's
// COALESCE actually picked. Seeding two rungs at once and asserting the result
// would pass against a ladder in any order.

// setUserName writes the TOP rung — the Prisma "User" row seedUser already made.
func (h *seedHelpers) setUserName(ctx context.Context, t *testing.T, userID, name string) {
	t.Helper()
	if _, err := h.pool.Exec(ctx,
		`UPDATE "User" SET "name" = $2 WHERE "id" = $1`, userID, name); err != nil {
		t.Fatalf("setUserName(%s): %v", userID, err)
	}
}

// seedAppleBinding writes the MIDDLE rung — the Apple first-consent name, the only
// rung an Apple-native account carries a real name on, and therefore the rung a
// `"User".name`-only lookup gets wrong.
func (h *seedHelpers) seedAppleBinding(ctx context.Context, t *testing.T, appleSub, userID, name string) {
	t.Helper()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO go_identity_apple (apple_sub, user_id, name) VALUES ($1, $2, NULLIF($3, ''))`,
		appleSub, userID, name); err != nil {
		t.Fatalf("seedAppleBinding(%s): %v", userID, err)
	}
}

// setGoUserName writes the BOTTOM rung. An UPSERT rather than an UPDATE: seedUser
// creates only the `"User"` row, so an Apple-native shape has no go_users row for
// an UPDATE to find.
func (h *seedHelpers) setGoUserName(ctx context.Context, t *testing.T, userID, name string) {
	t.Helper()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO go_users ("id", "name") VALUES ($1, $2)
		 ON CONFLICT ("id") DO UPDATE SET "name" = EXCLUDED."name"`, userID, name); err != nil {
		t.Fatalf("setGoUserName(%s): %v", userID, err)
	}
}

// confirmName records that the account CONFIRMED its display name (MYR-583) —
// the Go-owned `go_profile_name_confirmations` row that `PATCH /api/users/me`
// writes in the same transaction as the name.
//
// SEPARATE FROM THE THREE RUNG SEEDERS, and deliberately so: since MYR-583 a name
// on a rung is NOT enough to be shown to a counterparty or to make a car
// offerable, because the Apple rung is a first-consent payload nobody approved and
// the `"User"` rung may hold a legacy web placeholder. A test that wants a name the
// platform will actually PUBLISH seeds a rung AND calls this. A test that wants the
// legacy state — a resolvable name nobody confirmed — seeds a rung and does not.
func (h *seedHelpers) confirmName(ctx context.Context, t *testing.T, userID string) {
	t.Helper()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO go_profile_name_confirmations (user_id, confirmed_at) VALUES ($1, NOW())
		 ON CONFLICT (user_id) DO UPDATE SET confirmed_at = NOW()`, userID); err != nil {
		t.Fatalf("confirmName(%s): %v", userID, err)
	}
}

// vehicleSeed bundles the columns most contract tests want to set on a
// Vehicle row. Zero-valued fields fall through to their Prisma defaults
// (status=offline, charge=0, etc.) so a minimal call site can pass just
// the identifiers and let the schema fill the rest.
type vehicleSeed struct {
	ID                 string
	UserID             string
	VIN                string
	Name               string
	Model              string
	Year               int
	Color              string
	Status             string // empty string falls through to schema default 'offline'
	ChargeLevel        int
	EstimatedRange     int
	LocationName       string
	LocationAddress    string
	DestinationName    string // empty seeds NULL — the "not navigating" state
	DestinationAddress string // empty seeds NULL
	FsdMilesReset      float64
	LastUpdated        time.Time
	// Latitude/Longitude are the car's P1 position, planted as CIPHERTEXT ONLY
	// (MYR-433) exactly as the writers do. Both zero seeds the (0,0) no-fix
	// row, which is the state a car that has never reported a position is in —
	// and, since MYR-602, ALSO the state a narrowed viewer is shown, which is
	// the indistinguishability the sentinel rule is built on.
	Latitude  float64
	Longitude float64
}

// seedVehicle inserts a Vehicle row. The fixture covers every field the
// snapshot handler reads back (status, charge, name, address, etc.) —
// the wide-read path will scan all columns whether or not the test
// cares about them.
//
// MYR-447: the four geocoded labels are planted as CIPHERTEXT ONLY,
// which is the row shape production now writes — the retired plaintext
// columns get `”` / NULL exactly as vehicle_update_builder.go writes
// them. That is deliberate and it is what makes the label assertions in
// vehicle_snapshot_test.go load-bearing: a read path that regressed to
// the plaintext column would surface empty labels and fail, where a seed
// that wrote both halves would let the regression pass.
func (h *seedHelpers) seedVehicle(ctx context.Context, t *testing.T, v vehicleSeed) {
	t.Helper()
	status := v.Status
	if status == "" {
		status = "parked"
	}
	if v.LastUpdated.IsZero() {
		v.LastUpdated = time.Now().UTC()
	}
	_, err := h.pool.Exec(ctx, `
		INSERT INTO "Vehicle" (
			"id", "userId", "vin", "name",
			"model", "year", "color", "status",
			"chargeLevel", "estimatedRange",
			"locationName", "locationAddress",
			"locationNameEnc", "locationAddressEnc",
			"destinationNameEnc", "destinationAddressEnc",
			"latitudeEnc", "longitudeEnc",
			"fsdMilesSinceReset", "lastUpdated"
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8::"VehicleStatus",
			$9, $10,
			'', '',
			$11, $12,
			$13, $14,
			$15, $16,
			$17, $18
		)`,
		v.ID, v.UserID, v.VIN, v.Name,
		v.Model, v.Year, v.Color, status,
		v.ChargeLevel, v.EstimatedRange,
		h.seal(t, v.LocationName), h.seal(t, v.LocationAddress),
		h.seal(t, v.DestinationName), h.seal(t, v.DestinationAddress),
		h.sealFloat(t, v.Latitude), h.sealFloat(t, v.Longitude),
		v.FsdMilesReset, v.LastUpdated,
	)
	if err != nil {
		t.Fatalf("seedVehicle(%s): %v", v.ID, err)
	}
}

// seedFleetConfigAttempt plants one go_fleet_config_attempts row — the state
// behind the MYR-491 setupState derivation.
//
// Written as a raw INSERT rather than through the store writers on purpose:
// every writer supplies its own attempt_count and next_attempt_at, and a
// contract test wants to state the exact schedule shape it is asserting the
// WIRE projection of, not inherit whichever backoff the caller happened to
// compute. attempt_count and next_attempt_at take their schema defaults because
// the derivation deliberately reads neither.
func (h *seedHelpers) seedFleetConfigAttempt(
	ctx context.Context, t *testing.T,
	vehicleID, outcome string, lastAttemptAt time.Time,
) {
	t.Helper()
	_, err := h.pool.Exec(ctx, `
		INSERT INTO go_fleet_config_attempts (vehicle_id, last_outcome, last_attempt_at)
		VALUES ($1, $2, $3)`,
		vehicleID, outcome, lastAttemptAt)
	if err != nil {
		t.Fatalf("seedFleetConfigAttempt(%s): %v", vehicleID, err)
	}
}

// seal is the harness's single door to a sealed label (MYR-447). It
// delegates to testutil.SealLabel, which internal/store/db_test.go's
// sealCatalogLabels also uses, so a fixture planted here is
// byte-for-byte the shape the store tests plant and the writers write:
// ciphertext for a real label, NULL for an empty one.
func (h *seedHelpers) seal(t *testing.T, plain string) *string {
	t.Helper()
	ct, err := testutil.SealLabel(h.enc, plain)
	if err != nil {
		t.Fatalf("seal label: %v", err)
	}
	return ct
}

// sealFloat plants a coordinate the way the writers do: base64-GCM over the
// SHORTEST decimal that round-trips (strconv 'g', prec -1), which is the exact
// spelling store.floatToEncString produces. A different formatting would still
// decrypt, but a test asserting an exact wire value would then be asserting the
// seed's formatting rather than the read path's.
func (h *seedHelpers) sealFloat(t *testing.T, v float64) *string {
	t.Helper()
	ct, err := h.enc.EncryptString(strconv.FormatFloat(v, 'g', -1, 64))
	if err != nil {
		t.Fatalf("seal coordinate: %v", err)
	}
	return &ct
}

// seedAcceptedShare plants a LIVE accepted grant — the standing relationship a
// trip participant is chosen from (MYR-602). Accepted and unsuspended, which is
// the pair every access predicate on the platform asks for.
func (h *seedHelpers) seedAcceptedShare(ctx context.Context, t *testing.T, shareID, vehicleID, ownerID, granteeID string) {
	t.Helper()
	_, err := h.pool.Exec(ctx, `
		INSERT INTO go_vehicle_shares (
			id, vehicle_id, owner_user_id, label, permission, code,
			status, expires_at, accepted_at, accepted_by_user_id
		) VALUES ($1, $2, $3, 'Seeded', 'live', 'code_' || $1,
			'accepted', NOW() + INTERVAL '30 days', NOW(), $4)`,
		shareID, vehicleID, ownerID, granteeID)
	if err != nil {
		t.Fatalf("seedAcceptedShare(%s): %v", shareID, err)
	}
}

// suspendShare stamps suspended_at, leaving status 'accepted'. That combination
// is the whole point: a suspension is invisible in `status`, so an access
// predicate that checked only the status would keep admitting the grantee.
func (h *seedHelpers) suspendShare(ctx context.Context, t *testing.T, shareID string) {
	t.Helper()
	if _, err := h.pool.Exec(ctx,
		`UPDATE go_vehicle_shares SET suspended_at = NOW() WHERE id = $1`, shareID); err != nil {
		t.Fatalf("suspendShare(%s): %v", shareID, err)
	}
}

// tripSeed is one window on one car.
type tripSeed struct {
	ID        string
	VehicleID string
	OwnerID   string
	StartsAt  time.Time
	EndsAt    time.Time
}

// seedTrip plants a window. `name_enc` is sealed like every other P1 label —
// the column is NOT NULL with no plaintext sibling, so a seed cannot skip it.
func (h *seedHelpers) seedTrip(ctx context.Context, t *testing.T, tr tripSeed) {
	t.Helper()
	_, err := h.pool.Exec(ctx, `
		INSERT INTO go_trips (id, vehicle_id, owner_user_id, name_enc, starts_at, ends_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		tr.ID, tr.VehicleID, tr.OwnerID, *h.seal(t, "Seeded trip"), tr.StartsAt, tr.EndsAt)
	if err != nil {
		t.Fatalf("seedTrip(%s): %v", tr.ID, err)
	}
}

// seedTripParticipant puts one share-holder on a trip. The share id is carried
// rather than the user id alone because that IS the relationship: a trip
// creates no new grant, it decides what an existing one means between two
// instants.
func (h *seedHelpers) seedTripParticipant(ctx context.Context, t *testing.T, tripID, userID, shareID string) {
	t.Helper()
	_, err := h.pool.Exec(ctx, `
		INSERT INTO go_trip_participants (trip_id, user_id, share_id)
		VALUES ($1, $2, $3)`, tripID, userID, shareID)
	if err != nil {
		t.Fatalf("seedTripParticipant(%s/%s): %v", tripID, userID, err)
	}
}

// driveSeed bundles the columns that drive the drives-list ordering and
// pagination assertions. The store-layer ListByVehicleID reads
// startTime + id as the cursor anchor, so a test that wants
// deterministic pagination MUST set both per row.
//
// MYR-145 added Start/End Location + Address. Leave them empty to
// exercise the "drive in progress / not yet geocoded" branch (the wire
// payload omits the key entirely); set them to non-empty strings to
// assert the populated branch. MYR-447 changed only WHERE seedDrive puts
// them — sealed into the `*Enc` columns — not what the wire looks like.
type driveSeed struct {
	ID               string
	VehicleID        string
	Date             string
	StartTime        string
	EndTime          string
	StartLocation    string
	StartAddress     string
	EndLocation      string
	EndAddress       string
	DistanceMiles    float64
	DurationMinutes  int
	AvgSpeedMph      float64
	MaxSpeedMph      float64
	StartChargeLevel int
	EndChargeLevel   int
	CreatedAt        time.Time
}

// seedDrive inserts a Drive row.
//
// MYR-447: the four endpoint labels go to the `*Enc` columns only, and
// the retired plaintext columns get the four empty strings queryDriveInsert
// writes — the harness plants the same row a live drive detection now
// plants. An empty label seals to NULL, which is the absent sentinel the
// geocode backfill's discovery predicate keys on and the state the
// "not yet geocoded" case in vehicle_drives_test.go asserts against.
func (h *seedHelpers) seedDrive(ctx context.Context, t *testing.T, d driveSeed) {
	t.Helper()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	_, err := h.pool.Exec(ctx, `
		INSERT INTO "Drive" (
			"id", "vehicleId", "date", "startTime", "endTime",
			"startLocation", "startAddress", "endLocation", "endAddress",
			"startLocationEnc", "startAddressEnc", "endLocationEnc", "endAddressEnc",
			"distanceMiles", "durationMinutes",
			"avgSpeedMph", "maxSpeedMph",
			"startChargeLevel", "endChargeLevel",
			"createdAt"
		) VALUES (
			$1, $2, $3, $4, $5,
			'', '', '', '',
			$6, $7, $8, $9,
			$10, $11,
			$12, $13,
			$14, $15,
			$16
		)`,
		d.ID, d.VehicleID, d.Date, d.StartTime, d.EndTime,
		h.seal(t, d.StartLocation), h.seal(t, d.StartAddress),
		h.seal(t, d.EndLocation), h.seal(t, d.EndAddress),
		d.DistanceMiles, d.DurationMinutes,
		d.AvgSpeedMph, d.MaxSpeedMph,
		d.StartChargeLevel, d.EndChargeLevel,
		d.CreatedAt,
	)
	if err != nil {
		t.Fatalf("seedDrive(%s): %v", d.ID, err)
	}
}

// ---------------------------------------------------------------------------
// HTTP convenience helpers
// ---------------------------------------------------------------------------

// doGET issues a GET against the test server. When token is non-empty
// it sets the Authorization header — pass "" to exercise the missing-
// token branch.
func doGET(t *testing.T, srv *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// readBody reads the response body and closes it.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

// decodeJSON unmarshals body into v. Fails the test on parse error —
// every contract test SHOULD reach this point with a JSON body.
func decodeJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, string(body))
	}
}

// jsonMarshal is a thin wrapper around json.Marshal used by tests that
// want to re-serialize a decoded sub-object (e.g. a single list item)
// for schema validation. Exported as a package-private helper so call
// sites stay readable.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// ---------------------------------------------------------------------------
// JSON Schema validation
// ---------------------------------------------------------------------------

// validateAgainstSchema validates body against the schema at schemaPath
// (relative to the repo root, e.g. "docs/contracts/schemas/vehicle-state.schema.json").
//
// schemaPath MAY carry a JSON-pointer fragment to target a sub-schema —
// "docs/contracts/schemas/vehicle-summary.schema.json#/$defs/VehicleSummary"
// validates ONE ROW rather than the list envelope the file's root describes.
// The fragment form exists because several canonical schemas are envelope-rooted
// with the interesting object under $defs, and a test that has a single decoded
// item in hand should not have to re-wrap it.
//
// The compiler is loaded fresh per call — schemas are small and the
// jsonschema/v6 compiler caches them internally; perf is not a concern
// at test scale.
func validateAgainstSchema(t *testing.T, schemaPath string, body []byte) {
	t.Helper()

	filePath, fragment, hasFragment := strings.Cut(schemaPath, "#")

	root := repoRoot(t)
	schemasAbs := filepath.Join(root, "docs/contracts/schemas")
	mainSchema := filepath.Join(root, filePath)

	compiler := jsonschema.NewCompiler()
	loadSchemaResources(t, compiler, schemasAbs)

	mainID := schemaIDFromFile(t, mainSchema)
	if hasFragment {
		mainID += "#" + fragment
	}
	schema, err := compiler.Compile(mainID)
	if err != nil {
		t.Fatalf("compile schema %s: %v", schemaPath, err)
	}

	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("unmarshal body for schema validation: %v\nbody: %s", err, string(body))
	}
	if err := schema.Validate(parsed); err != nil {
		t.Errorf("schema validation failed (%s):\n%v\nbody: %s", schemaPath, err, string(body))
	}
}

// loadSchemaResources adds every schema under schemasAbs to the
// compiler so $ref resolution works across files even when the test
// only validates against one of them.
func loadSchemaResources(t *testing.T, c *jsonschema.Compiler, schemasAbs string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(schemasAbs, "*.json"))
	if err != nil {
		t.Fatalf("glob schemas: %v", err)
	}
	for _, f := range files {
		raw, err := readSchemaJSON(f)
		if err != nil {
			t.Fatalf("read schema %s: %v", f, err)
		}
		id, _ := raw.(map[string]any)["$id"].(string)
		if id == "" {
			id = "file:///" + filepath.Base(f)
		}
		if err := c.AddResource(id, raw); err != nil {
			t.Fatalf("add resource %s: %v", f, err)
		}
	}
}

// schemaIDFromFile reads the $id field from a schema file. The main
// compile target uses the $id so $ref-by-id works alongside file-based
// loading.
func schemaIDFromFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := readSchemaJSON(path)
	if err != nil {
		t.Fatalf("read schema %s: %v", path, err)
	}
	id, _ := raw.(map[string]any)["$id"].(string)
	if id == "" {
		t.Fatalf("schema %s has no $id", path)
	}
	return id
}

func readSchemaJSON(path string) (any, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- test-local schema path
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(data))
}

// NOTE: `repoRoot` is provided by tests/contract/fixtures_test.go,
// which is in the same package and compiled in both default and
// contract-tagged builds. We deliberately don't re-declare it here.

// ---------------------------------------------------------------------------
// Error envelope helpers
// ---------------------------------------------------------------------------

// restErrorEnvelope mirrors the rest-api.md §4.1 error shape returned
// by `wserrors.WriteErrorEnvelope`. Tests decode response bodies into
// this struct to assert on the typed `error.code`.
type restErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// assertErrorCode decodes body and checks the typed error code matches
// wantCode (e.g. "auth_failed", "not_found"). Fails the test with the
// full body on mismatch so a regression points straight at the wire
// shape.
func assertErrorCode(t *testing.T, body []byte, wantCode string) {
	t.Helper()
	var env restErrorEnvelope
	decodeJSON(t, body, &env)
	if env.Error.Code != wantCode {
		t.Errorf("error.code = %q, want %q\nbody: %s", env.Error.Code, wantCode, string(body))
	}
}
