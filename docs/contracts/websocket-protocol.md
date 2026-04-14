# WebSocket Protocol Contract

**Status:** Draft -- v1
**Target artifact:** AsyncAPI 3.0 specification at [`specs/websocket.asyncapi.yaml`](specs/websocket.asyncapi.yaml)
**Owner:** `sdk-architect` agent
**Last updated:** 2026-04-13

## Purpose

Defines every WebSocket message exchanged between the telemetry server (`internal/ws/`) and SDK clients (TypeScript, Swift, web, mobile). This contract is the authoritative source for:

- The connection handshake (auth frame, token validation, accept/reject)
- Server->client and client->server message catalogs
- The envelope schema (type discriminator, payload, planned sequence/timestamp)
- Heartbeat cadence and close-code semantics
- Reconnection and snapshot-resume rules

The markdown is the human source of truth. Its machine-readable twin is [`specs/websocket.asyncapi.yaml`](specs/websocket.asyncapi.yaml). Per-message JSON Schemas live alongside in [`schemas/`](schemas/). Drift between this doc, the AsyncAPI spec, the schemas, and `internal/ws/` is a CI failure ([`contract-guard`](../../CLAUDE.md#merge-policy-non-negotiable)).

## Anchored requirements

| ID | Requirement | Where it lands in this doc |
|----|-------------|----------------------------|
| **FR-1.1** | Live telemetry stream: position, speed, heading, gear | §4.1 vehicle_update; §4.1.3 GPS group; §4.1.5 gear group |
| **FR-1.2** | Live charge state: battery level, charge state, range | §4.1.4 charge group |
| **FR-1.3** | Architecture allows new telemetry fields without architectural change | §4.1 (extensibility), §10 |
| **FR-2.1** | Nav: destination, ETA, distance, polyline, origin, trip start | §4.1.2 navigation group |
| **FR-2.2** | Nav fields delivered as an atomic group | §4.1.2; §3.2 atomic-group rule |
| **FR-2.3** | Nav cancellation clears the entire group atomically | §4.1.2 nav clear; §3.3 clear semantics |
| **FR-3.1** | Live drive events: drive_started, drive_updated, drive_ended | §4.2 drive_started; §4.3 drive_ended; §4.1.6 drive_updated routing |
| **FR-6.1** | SDK accepts a getToken() callback | §2.1 handshake; §5.1 auth message |
| **FR-7.1** | Typed error codes (no string-matching) | §6.1 error frame; §6.2 close codes |
| **FR-8.1** | connectionState surface | §2 handshake; §7 reconnect |
| **FR-8.2** | dataState per atomic group, independent | §4.1 group routing; §4.5 group/clear signaling |
| **NFR-3.1** | Atomic grouping of related fields | §3.2 atomic-group rule; §4.1 catalog |
| **NFR-3.2** | 200ms server-side debounce window | §3.2.1 debounce note (and 500ms drift) |
| **NFR-3.10** | Reconnect with exponential backoff (1s/2x/30s/jitter) | §7.1 backoff parameters |
| **NFR-3.11** | Reconnect re-fetches DB snapshot, resumes live stream | §7.2 reconnect sequence; §7.3 snapshot-resume semantics |
| **NFR-3.21** | Vehicle ownership enforced on every subscription | §2.2 server-side ownership check |
| **NFR-3.22** | TLS in transit (WSS for browsers/apps) | §2 transport |

---

## 1. Transport and URL

### 1.1 Endpoint

| Field | Value |
|-------|-------|
| Path | `/api/ws` (registered in [`internal/ws/handler.go`](../../internal/ws/handler.go)) |
| Production scheme | `wss://` (TLS termination at the edge, NFR-3.22) |
| Local dev scheme | `ws://` (allowed only for `localhost`/`127.0.0.1` origins) |
| HTTP method | `GET` (WebSocket upgrade) |
| Content type | `application/json`, framed as WebSocket text messages |
| Upgrade library | [`github.com/coder/websocket`](https://github.com/coder/websocket) (NEVER `gorilla/websocket` -- unmaintained per `CLAUDE.md`) |

### 1.2 Origin enforcement

The server passes `WebSocketConfig.AllowedOrigins` to `websocket.AcceptOptions.OriginPatterns`. Cross-origin requests from origins not in the allow-list are rejected with HTTP 403 BEFORE the upgrade completes. There is no in-band error frame for this case -- the client receives an HTTP error response on the upgrade attempt.

| Mode | Default | Source |
|------|---------|--------|
| Production | Configured allow-list (`https://app.myrobotaxi.com`, etc.) | `cfg.WebSocket().AllowedOrigins` |
| Dev | `["*"]` (allow all) | `cmd/telemetry-server/main.go` fallback |

### 1.3 Per-IP connection cap

The handler enforces an optional per-IP connection limit (`HandlerConfig.MaxConnectionsPerIP`). When the cap is reached, new upgrade attempts are rejected with HTTP 429 BEFORE the WebSocket is opened. SDKs MUST treat HTTP 429 on the upgrade as `rate_limited` and apply the standard reconnect backoff (NFR-3.10). The client IP is resolved from the leftmost `X-Forwarded-For` entry when present (`internal/ws/handler.go:resolveClientIP`).

---

## 2. Connection handshake

> **Anchored:** FR-6.1, FR-6.2, NFR-3.21, NFR-3.22, FR-8.1.

### 2.1 Sequence

```
Client                                       Server
  |                                            |
  |--- HTTP GET /api/ws (Upgrade: websocket) ->|
  |                                            |
  |                              [origin check]
  |                              [per-IP cap check]
  |                                            |
  |<-- HTTP 101 Switching Protocols -----------|
  |                                            |
  |                              [auth deadline starts: 5s]
  |                                            |
  |--- {"type":"auth","payload":{"token":"..."}} ->
  |                                            |
  |                              [Authenticator.ValidateToken]
  |                              [Authenticator.GetUserVehicles]
  |                                            |
  |  -- success path --                        |
  |                              [Hub.Register, start read+write pumps]
  |                                            |
  |   <-- vehicle_update / drive_* / heartbeat |   (live stream begins)
  |                                            |
  |  -- failure path --                        |
  |<-- {"type":"error","payload":{"code":"auth_failed",...}}
  |<-- WebSocket close code 1008 (policy violation)
```

Implementation: [`internal/ws/handler.go`](../../internal/ws/handler.go) `handleUpgrade` -> `authenticateClient`.

### 2.2 Auth frame requirements

1. **First frame.** The client MUST send the auth frame as its FIRST WebSocket frame after the upgrade. Any other frame type before auth is treated as an auth failure.
2. **No HTTP header.** The token MUST NOT be sent as an `Authorization` header on the upgrade request. Browsers cannot set arbitrary headers on the WebSocket upgrade, so the in-band frame is the only portable channel. (Native clients MAY send a duplicate header for defense-in-depth, but the server ignores it.)
3. **Deadline.** The server enforces a 5-second deadline (`HandlerConfig.AuthTimeout`, default `5*time.Second`) for the auth frame. Exceeding the deadline produces error code `auth_timeout` and close code 1008.
4. **Token format.** Opaque to the WebSocket layer. The configured `Authenticator` validates it. Production uses [`internal/auth.NewJWTAuthenticator`](../../internal/auth) which checks signature, issuer, audience, and expiry. Dev uses [`ws.NoopAuthenticator`](../../internal/ws/auth.go) which accepts any non-empty token.
5. **Vehicle resolution.** On a valid token, the server calls `Authenticator.GetUserVehicles(ctx, userID)` and stores the resulting vehicle IDs on the `Client` struct. The set is a snapshot of ownership at handshake time; per-broadcast ownership filtering uses this snapshot (§4.4).
6. **Token redaction.** The token is **P1** per [`data-classification.md`](data-classification.md) §1.2. It MUST NOT appear in any structured log, error message, metric label, or crash report. The server never logs the token value -- only the resulting `userID` (P0) and `vehicle_count` (P0).

### 2.3 Auth result envelope

The server does **not** send a success acknowledgement frame. Successful auth is implicit: the next frames the client receives are normal `vehicle_update` / `heartbeat` traffic.

The server **does** send an explicit error frame on failure (§6.1) followed by a close frame with code 1008. The SDK MUST tolerate the absence of an acknowledgement on success.

> **PLANNED:** A future revision will introduce an `auth_ok` frame echoing the resolved `userID`, vehicle count, and (when sequence numbers ship per NFR-3.11) the initial server `seq`. Tracked as a follow-up (§10).

### 2.4 Connection state mapping

The handshake drives the following [`state-machine.md`](state-machine.md) transitions:

| Wire event | `connectionState` transition | Notes |
|------------|------------------------------|-------|
| HTTP 101 + `auth` frame sent | `connecting` (no change yet) | Token in flight |
| `Authenticator.ValidateToken` returns success | `connecting -> connected` (C-3) | Reset retry counter, start heartbeat watchdog |
| `Authenticator.ValidateToken` returns error | `connecting -> error` (C-8) | Surface `auth_failed` |
| Auth deadline exceeded | `connecting -> error` (C-8) | Surface `auth_timeout` |
| HTTP 429 on upgrade | `connecting -> disconnected` (C-4) | Apply backoff |
| HTTP 403 on upgrade (origin) | `connecting -> error` (C-5) | Terminal -- consumer must fix origin config |

---

## 3. Envelope schema

> **Anchored:** FR-1.3 (extensibility), NFR-3.1, NFR-3.11.
>
> **Schema reference:** [`schemas/ws-envelope.schema.json`](schemas/ws-envelope.schema.json) and [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json).

### 3.1 Wire shape

Every frame is a JSON object with the following top-level keys:

```jsonc
{
  "type": "vehicle_update",   // string discriminator (required)
  "payload": { ... },         // type-specific object (omitted on bare control frames)
  "seq":  42,                 // PLANNED: monotonic per-connection sequence (NFR-3.11)
  "ts":   "2026-04-13T18:22:01Z" // PLANNED: server-authoritative envelope timestamp
}
```

| Field | Required (today) | Direction | Description |
|-------|------------------|-----------|-------------|
| `type` | YES | both | Discriminator. See §4 (server->client) and §5 (client->server) for the catalog. |
| `payload` | Per-type | both | Type-specific object. Omitted on `heartbeat` (server emits a bare envelope -- see [`internal/ws/heartbeat.go`](../../internal/ws/heartbeat.go)). |
| `seq` | NO (PLANNED) | server->client | Monotonic per-connection sequence number. NOT yet emitted. See §3.3. |
| `ts` | NO (PLANNED) | server->client | Server-authoritative ISO 8601 UTC envelope timestamp. NOT yet emitted at the envelope level; `vehicle_update.payload.timestamp`, `drive_started.payload.timestamp`, etc. carry the same information today. See §3.3. |

The Go struct that produces this envelope is [`internal/ws/messages.go:wsMessage`](../../internal/ws/messages.go):

```go
type wsMessage struct {
    Type    string          `json:"type"`
    Payload json.RawMessage `json:"payload,omitempty"`
}
```

### 3.2 Atomic-group rule (NFR-3.1)

A single `vehicle_update` frame's `payload.fields` map MUST contain members of **at most one** atomic group, plus any number of individually-delivered fields. The atomic groups are defined in [`vehicle-state-schema.md`](vehicle-state-schema.md) §2:

| Group | Members |
|-------|---------|
| `navigation` | `destinationName`, `destinationAddress`*, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`, `etaMinutes`, `tripDistanceRemaining`, `navRouteCoordinates` |
| `charge` | `chargeLevel`, `estimatedRange` |
| `gps` | `latitude`, `longitude`, `heading` |
| `gear` | `gearPosition`, `status` |

\* `destinationAddress` is currently spec-only (MYR-24); see [`vehicle-state-schema.md`](vehicle-state-schema.md) §1.1.

Individually-delivered fields (no atomic group): `speed`, `odometerMiles`, `interiorTemp`, `exteriorTemp`, `fsdMilesToday`, `locationName`, `locationAddress`, `lastUpdated`, and the drive-only `routeCoordinates` field (§4.1.6).

The server enforces this rule by routing nav-group fields through the `navAccumulator` ([`internal/ws/nav_accumulator.go`](../../internal/ws/nav_accumulator.go)) and flushing them as a single message; non-nav fields broadcast immediately. SDKs MUST validate the rule on receipt and treat a frame containing fields from two different atomic groups as a contract violation (log + reject).

#### 3.2.1 Debounce window (NFR-3.2)

NFR-3.2 specifies a **200ms** server-side debounce window. The current navigation accumulator implementation uses **500ms** (`defaultNavFlushInterval` in [`internal/ws/nav_accumulator.go`](../../internal/ws/nav_accumulator.go) line 12) because Tesla emits `RouteLine` and `MinutesToArrival` on independent ~1-second tickers and a 200ms window produces incomplete batches in practice. This drift between requirements text and implementation is documented in [`vehicle-state-schema.md`](vehicle-state-schema.md) §2.1 ("Server-side implementation: ... 500ms flush window") and is tracked in §10 of this doc.

For the purpose of the protocol contract: SDKs MUST NOT assume timing finer than the debounce window. Two updates to the same atomic group within 500ms MAY be coalesced into one frame.

### 3.3 Sequence and timestamp (NFR-3.11) -- gap from spec

The Linear AC for MYR-11 calls for a "type discriminator, sequence, timestamp" envelope. Today only the `type` discriminator and per-payload `timestamp` field exist. The envelope-level `seq` and `ts` fields are **PLANNED** for a follow-up issue (see §10). The reasons:

1. The current server has no per-connection sequence counter ([`internal/ws/client.go:Client`](../../internal/ws/client.go) struct exposes `userID`, `vehicleIDs`, `send`, `remoteAddr`, `hub`, `logger` -- no `nextSeq` field).
2. Reconnect-resume is currently implemented entirely client-side via REST snapshot fetch (see [`state-machine.md`](state-machine.md) §5), which establishes a consistent baseline without needing wire-level sequence numbers. The trade-off: clients cannot detect dropped frames within a connection.
3. Adding `seq` requires a coordinated server + SDK change AND a fixture migration. It is a v1.x extension, not a v1.0 ship blocker.

This doc and the AsyncAPI spec describe the **target shape** including `seq` and `ts` so SDK consumers can plan for it. Until the server emits these fields, SDKs MUST tolerate their absence -- per JSON's open-object semantics, an unknown future field appearing on the wire is also acceptable.

> **SDK MUST:** Treat `seq` and `ts` as optional today. When they appear, prefer envelope `ts` over payload `timestamp` for ordering.
>
> **SDK SHOULD:** Maintain a per-connection "highest seq seen" counter as soon as the server begins emitting `seq`, and pass it as `subscribe.sinceSeq` (§5.2) on reconnect.

### 3.4 Frame size

The server enforces a 4 KiB read limit on inbound (client->server) frames via `Conn.SetReadLimit(readLimit)` ([`internal/ws/client.go:readLimit`](../../internal/ws/client.go) line 19). The expected client traffic is exclusively `auth` plus future control frames -- 4 KiB is generous.

Outbound (server->client) frames have no hard cap. Long `navRouteCoordinates` payloads from city-scale Tesla routes are the largest realistic frames (~5-15 KB serialized). SDKs MUST tolerate frames up to 1 MB without truncation.

---

## 4. Server -> client message catalog

> **Anchored:** FR-1.1, FR-1.2, FR-1.3, FR-2.1, FR-2.2, FR-2.3, FR-3.1, FR-7.1, FR-8.1, FR-8.2, NFR-3.1, NFR-3.2.

This section is the wire-level catalog. Field-level types, units, nullability, and per-field data classification live in [`vehicle-state-schema.md`](vehicle-state-schema.md) and the canonical JSON Schema ([`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json)) -- they are not duplicated here.

### Catalog summary

| `type` | Direction | Source (Go) | Atomic group | Triggers `dataState` transition | Fixture |
|--------|-----------|-------------|--------------|---------------------------------|---------|
| `vehicle_update` | server->client | [`broadcaster.go` / `nav_broadcast.go` / `route_broadcast.go`](../../internal/ws/) | one of `navigation`, `charge`, `gps`, `gear`, or none | per-group `ready/cleared/error -> ready` | [`fixtures/websocket/vehicle_update.charge.json`](fixtures/websocket/vehicle_update.charge.json) (TODO) |
| `drive_started` | server->client | [`broadcaster.go:handleDriveStarted`](../../internal/ws/broadcaster.go) | n/a | drive lifecycle `idle -> driving` | [`fixtures/websocket/drive_started.json`](fixtures/websocket/drive_started.json) (TODO) |
| `drive_ended` | server->client | [`broadcaster.go:handleDriveEnded`](../../internal/ws/broadcaster.go) | n/a | drive lifecycle `driving -> ended` | [`fixtures/websocket/drive_ended.json`](fixtures/websocket/drive_ended.json) (TODO) |
| `connectivity` | server->client | [`broadcaster.go:handleConnectivity`](../../internal/ws/broadcaster.go) | n/a | none (vehicle<->server signal, not SDK<->server) | [`fixtures/websocket/connectivity.json`](fixtures/websocket/connectivity.json) (TODO) |
| `heartbeat` | server->client | [`heartbeat.go`](../../internal/ws/heartbeat.go) | n/a | resets liveness watchdog | [`fixtures/websocket/heartbeat.json`](fixtures/websocket/heartbeat.json) (TODO) |
| `error` | server->client | [`handler.go:sendError`](../../internal/ws/handler.go) | n/a | `connecting -> error` (during handshake) | [`fixtures/websocket/error.auth_failed.json`](fixtures/websocket/error.auth_failed.json) (TODO) |

All fixture paths are TODO entries in [`fixtures/README.md`](fixtures/README.md). Adding the actual fixture JSON files is tracked in a follow-up (see §10).

### 4.1 `vehicle_update`

> **Anchored:** FR-1.1, FR-1.2, FR-1.3, FR-2.1, FR-2.2, FR-2.3, NFR-3.1, NFR-3.2, NFR-3.5, NFR-3.9.
>
> **Schema:** [`schemas/ws-messages.schema.json#/$defs/VehicleUpdatePayload`](schemas/ws-messages.schema.json)

The primary telemetry payload. One frame carries field updates for one vehicle, scoped to at most one atomic group.

```jsonc
{
  "type": "vehicle_update",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "fields": {
      // members of AT MOST one atomic group + any individually-delivered fields
    },
    "timestamp": "2026-04-13T18:22:01Z"
  }
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `payload.vehicleId` | `string` (cuid) | **P0** | Opaque DB ID. NEVER VIN. FR-4.2. |
| `payload.fields` | `object` | mixed (per-field) | See [`vehicle-state-schema.md`](vehicle-state-schema.md) §1.1 for per-field classification. |
| `payload.timestamp` | `string` (ISO 8601 UTC) | **P0** | Server `time.Now().UTC()` at broadcast (or telemetry `CreatedAt` for non-nav fields). |

#### 4.1.1 Field name translation

The wire field names in `payload.fields` are the **frontend** field names, not Tesla's protobuf names. The server translates internal telemetry names to client names in [`internal/ws/field_mapping.go:internalToClientField`](../../internal/ws/field_mapping.go):

| Tesla / internal name | Wire name |
|-----------------------|-----------|
| `soc` | `chargeLevel` |
| `gear` | `gearPosition` |
| `odometer` | `odometerMiles` |
| `insideTemp` | `interiorTemp` |
| `outsideTemp` | `exteriorTemp` |
| `minutesToArrival` | `etaMinutes` |
| `milesToArrival` | `tripDistanceRemaining` |
| `fsdMilesSinceReset` | `fsdMilesToday` |
| `location` | split into `latitude` + `longitude` |
| `destinationLocation` | split into `destinationLatitude` + `destinationLongitude` |
| `originLocation` | split into `originLatitude` + `originLongitude` |
| `routeLine` (Tesla polyline) | `navRouteCoordinates` (decoded `[lng, lat]` array) |
| `hvacFanSpeed` | `fanSpeed` |

Integer rounding is applied server-side to fields listed in `integerFields` (`speed`, `heading`, `chargeLevel`, `estimatedRange`, `etaMinutes`, `interiorTemp`, `exteriorTemp`, `odometerMiles`, ...).

#### 4.1.2 Navigation group

> **Anchored:** FR-2.1, FR-2.2, FR-2.3, NFR-3.1, NFR-3.2, NFR-3.9.
> **dataState target:** `dataState.navigation` (per [`state-machine.md`](state-machine.md) §2)
> **Fixture:** [`fixtures/websocket/vehicle_update.nav_active.json`](fixtures/websocket/vehicle_update.nav_active.json) (TODO), [`fixtures/websocket/vehicle_update.nav_clear.json`](fixtures/websocket/vehicle_update.nav_clear.json) (TODO)

Members (per [`vehicle-state-schema.md`](vehicle-state-schema.md) §2.1): `destinationName`, `destinationAddress`, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`, `etaMinutes`, `tripDistanceRemaining`, `navRouteCoordinates`.

| Field | Classification | Encrypted at rest |
|-------|----------------|-------------------|
| `destinationName` | **P1** | No (disk encryption only) |
| `destinationAddress` | **P1** (spec-only, MYR-24) | No |
| `destinationLatitude` | **P1** | Yes (AES-256-GCM) |
| `destinationLongitude` | **P1** | Yes |
| `originLatitude` | **P1** | Yes |
| `originLongitude` | **P1** | Yes |
| `etaMinutes` | **P0** | No |
| `tripDistanceRemaining` | **P0** | No |
| `navRouteCoordinates` | **P1** | Yes |

**Server flow:** All nav-related fields are routed through [`navAccumulator.Add`](../../internal/ws/nav_accumulator.go). The first nav field for a VIN starts a 500ms timer (see §3.2.1 for the 200ms-vs-500ms drift). Subsequent nav fields within the window are merged. On timer expiry (or on `drive_ended` / `connectivity:offline`), the broadcaster flushes the accumulated batch as a single `vehicle_update`.

**Nav clear (FR-2.3):** When Tesla cancels navigation, it marks the corresponding telemetry fields as `Invalid`. The server translates each invalid nav field to a JSON `null` value in `payload.fields`. The mapping is defined by [`navClearFields`](../../internal/ws/field_mapping.go) line 78:

| Internal field invalidated | Client fields set to `null` |
|----------------------------|------------------------------|
| `destinationName` | `destinationName` |
| `milesToArrival` | `tripDistanceRemaining` |
| `minutesToArrival` | `etaMinutes` |
| `routeLine` | `navRouteCoordinates` |
| `originLocation` | `originLatitude`, `originLongitude` |
| `destinationLocation` | `destinationLatitude`, `destinationLongitude` |

**SDK requirement:** Per NFR-3.9 and Rule CG-SM-3 ([`state-machine.md`](state-machine.md) §7), when ANY navigation field arrives as `null`, the SDK MUST atomically null ALL navigation group fields in its in-memory state, regardless of whether the server explicitly sent every member. The server is permitted to send a partial clear (e.g., just `destinationName: null`) -- the SDK is responsible for amplifying it to the full group.

#### 4.1.3 GPS group

> **Anchored:** FR-1.1, NFR-3.1.
> **dataState target:** `dataState.gps`
> **Fixture:** [`fixtures/websocket/vehicle_update.gps.json`](fixtures/websocket/vehicle_update.gps.json) (TODO)

Members: `latitude`, `longitude`, `heading`.

| Field | Classification | Encrypted at rest |
|-------|----------------|-------------------|
| `latitude` | **P1** | Yes (AES-256-GCM) |
| `longitude` | **P1** | Yes |
| `heading` | **P0** | No |

**Server flow:** When Tesla emits a `Location` (compound `{lat, lng}`) field, the server splits it into separate `latitude` and `longitude` keys (`splitLocationField` in field_mapping.go). `heading` is delivered alongside whenever Tesla emits `GpsHeading`. There is no nav-style accumulator for GPS -- updates broadcast immediately.

**0,0 sentinel:** Per [`vehicle-state-schema.md`](vehicle-state-schema.md) §2.3, the DB default for unset coordinates is `(0, 0)`. SDKs MUST treat `latitude == 0 && longitude == 0` as "no fix" rather than a valid point in the Gulf of Guinea.

#### 4.1.4 Charge group

> **Anchored:** FR-1.2, NFR-3.1.
> **dataState target:** `dataState.charge`
> **Fixture:** [`fixtures/websocket/vehicle_update.charge.json`](fixtures/websocket/vehicle_update.charge.json) (TODO)

Members: `chargeLevel`, `estimatedRange`.

| Field | Classification |
|-------|----------------|
| `chargeLevel` | **P0** |
| `estimatedRange` | **P0** |

Both fields are P0 (no encryption, log-safe). Tesla emits these on a 30-second cadence. The broadcaster delivers them in the same `vehicle_update` whenever either changes.

#### 4.1.5 Gear group

> **Anchored:** NFR-3.1.
> **dataState target:** `dataState.gear`
> **Fixture:** [`fixtures/websocket/vehicle_update.gear.json`](fixtures/websocket/vehicle_update.gear.json) (TODO)

Members: `gearPosition`, `status`.

| Field | Classification |
|-------|----------------|
| `gearPosition` | **P0** |
| `status` | **P0** |

`status` is **derived server-side** by [`deriveVehicleStatus`](../../internal/ws/field_mapping.go) from `gearPosition` (and `speed` as a fallback when gear is missing). The broadcaster injects `status` into the `vehicle_update` whenever `gearPosition` is present in the same frame:

```go
if _, hasGear := fields["gearPosition"]; hasGear {
    fields["status"] = deriveVehicleStatus(fields)
}
```

Per [`vehicle-state-schema.md`](vehicle-state-schema.md) §3.4, the SDK MUST validate the gear-to-status derivation predicate on receipt: `D` or `R` => `driving`, `P` or `N` => `parked` (unless overridden by `charging`/`offline`/`in_service`).

#### 4.1.6 Drive route updates (`drive_updated` is virtual)

> **Anchored:** FR-3.1.
> **dataState target:** `dataState.gps`
> **Drive lifecycle target:** `driving -> driving` (DR-2)
> **Fixture:** [`fixtures/websocket/vehicle_update.route.json`](fixtures/websocket/vehicle_update.route.json) (TODO)

Per [`state-machine.md`](state-machine.md) §4.1, `drive_updated` is **NOT a distinct wire message**. During an active drive, the broadcaster's [`handleDriveUpdated`](../../internal/ws/route_broadcast.go) appends each GPS point to a per-VIN [`routeAccumulator`](../../internal/ws/route_accumulator.go). When the accumulator hits its batch threshold (5 new points by default) or its flush interval (3 seconds by default), the broadcaster sends a `vehicle_update` whose `payload.fields` contains a **single key**:

```jsonc
{
  "type": "vehicle_update",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "fields": {
      "routeCoordinates": [[-122.4194, 37.7749], [-122.4193, 37.7750], ...]
    },
    "timestamp": "2026-04-13T18:23:05Z"
  }
}
```

`routeCoordinates` is `[lng, lat]` order (GeoJSON / Mapbox), distinct from the navigation group's `navRouteCoordinates`. The route accumulator's buffer is **not cleared on flush** -- each batch contains the **complete driven path** so the SDK can render the full polyline by replacing rather than appending. The buffer is cleared only on `drive_ended`.

**SDK requirement:** When a `vehicle_update` carrying `routeCoordinates` arrives during an active drive, the SDK MUST emit `drive_updated` as a logical event to consumers AND merge the array into its in-memory drive state. Per Rule CG-SM-6 in [`state-machine.md`](state-machine.md) §7, the SDK MUST NOT synthesize `drive_updated` from any other source.

### 4.2 `drive_started`

> **Anchored:** FR-3.1.
> **Drive lifecycle target:** `idle -> driving` (DR-1) or `ended -> driving` (DR-6)
> **Schema:** [`schemas/ws-messages.schema.json#/$defs/DriveStartedPayload`](schemas/ws-messages.schema.json)
> **Fixture:** [`fixtures/websocket/drive_started.json`](fixtures/websocket/drive_started.json) (TODO)

```jsonc
{
  "type": "drive_started",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "driveId": "clmno9876543210zyxw",
    "startLocation": {
      "latitude": 37.7749,
      "longitude": -122.4194
    },
    "timestamp": "2026-04-13T18:22:00Z"
  }
}
```

| Field | Classification |
|-------|----------------|
| `vehicleId` | **P0** |
| `driveId` | **P0** (matches the eventual persisted `Drive.id`) |
| `startLocation.latitude` | **P1** (encrypted at rest in the `Drive.routePoints` JSONB, plaintext on the wire under WSS per NFR-3.22) |
| `startLocation.longitude` | **P1** |
| `timestamp` | **P0** |

### 4.3 `drive_ended`

> **Anchored:** FR-3.1, FR-3.4.
> **Drive lifecycle target:** `driving -> ended` (DR-3)
> **Schema:** [`schemas/ws-messages.schema.json#/$defs/DriveEndedPayload`](schemas/ws-messages.schema.json)
> **Fixture:** [`fixtures/websocket/drive_ended.json`](fixtures/websocket/drive_ended.json) (TODO)

```jsonc
{
  "type": "drive_ended",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "driveId": "clmno9876543210zyxw",
    "distance": 12.4,
    "duration": "24m18s",
    "avgSpeed": 30.5,
    "maxSpeed": 65.2,
    "timestamp": "2026-04-13T18:46:18Z"
  }
}
```

| Field | Type | Classification |
|-------|------|----------------|
| `vehicleId` | `string` | **P0** |
| `driveId` | `string` | **P0** |
| `distance` | `number` (miles) | **P0** |
| `duration` | `string` (Go `time.Duration` format, e.g., `24m18s`) | **P0** |
| `avgSpeed` | `number` (mph) | **P0** |
| `maxSpeed` | `number` (mph) | **P0** |
| `timestamp` | `string` (ISO 8601 UTC) | **P0** |

> **Scope note:** Per [`state-machine.md`](state-machine.md) §4.1, the WebSocket `drive_ended` payload contains only the summary fields above. The full FR-3.4 record (energy used, FSD miles, intervention count, start/end charge level, start/end addresses, full route polyline) is fetched via REST `/drives/{id}` (see [`rest-api.md`](rest-api.md)). The SDK SHOULD fire a REST fetch on `drive_ended` if the consumer needs the full record.

> **Micro-drive filter:** Drives that fail the micro-drive filter (default 2 minutes / 0.1 miles, see [`state-machine.md`](state-machine.md) §3.5) NEVER produce a `drive_ended` frame. The SDK relies on `WS_DISCONNECTED` (transition DR-4) or an extended absence of route updates as the only signal that an in-progress drive has been suppressed.

### 4.4 `connectivity`

> **Schema:** [`schemas/ws-messages.schema.json#/$defs/ConnectivityPayload`](schemas/ws-messages.schema.json)
> **Fixture:** [`fixtures/websocket/connectivity.online.json`](fixtures/websocket/connectivity.online.json) (TODO), [`fixtures/websocket/connectivity.offline.json`](fixtures/websocket/connectivity.offline.json) (TODO)

```jsonc
{
  "type": "connectivity",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "online": true,
    "timestamp": "2026-04-13T18:22:00Z"
  }
}
```

| Field | Classification |
|-------|----------------|
| `vehicleId` | **P0** |
| `online` | **P0** |
| `timestamp` | **P0** |

**Important distinction:** `connectivity.online` reports the **vehicle<->server** (Tesla mTLS) connection status, NOT the **client<->server** (WebSocket) status. The latter is implicit in the WebSocket connection itself (an absent connection IS the disconnected state).

Per [`state-machine.md`](state-machine.md) §4.2, `connectivity` does NOT directly transition `connectionState` -- the SDK already knows its WebSocket is open, since it just received a frame on it. The signal is informational: the UI may show "Vehicle offline" while continuing to display cached data. When the server emits `connectivity.online: false`, the broadcaster also clears any pending nav accumulator state for that VIN to prevent stale nav data on reconnect ([`broadcaster.go:handleConnectivity`](../../internal/ws/broadcaster.go) line 227).

### 4.5 Per-vehicle ownership filtering

> **Anchored:** NFR-3.21.

`Hub.Broadcast(vehicleID, msg)` ([`internal/ws/hub.go`](../../internal/ws/hub.go) line 70) iterates every connected client and calls `client.hasVehicle(vehicleID)`. Only clients whose `vehicleIDs` slice (populated at handshake time from `Authenticator.GetUserVehicles`) contains the target vehicle ID receive the frame. Clients with an empty vehicle list (the `NoopAuthenticator` dev mode) receive ALL broadcasts.

The SDK can rely on this contract: **a client will NEVER receive a `vehicle_update`, `drive_started`, `drive_ended`, or `connectivity` frame for a vehicle it does not own.** If ownership changes after the handshake (e.g., an invite is revoked), the change takes effect only on the next reconnection -- the in-memory `vehicleIDs` snapshot is not refreshed mid-connection. This is a known limitation and is tracked as a follow-up in §10.

`heartbeat` frames are broadcast to ALL clients regardless of vehicle ownership ([`Hub.BroadcastAll`](../../internal/ws/hub.go) line 90).

---

## 5. Client -> server message catalog

> **Anchored:** FR-6.1, FR-6.2, NFR-3.21.

### Catalog summary

| `type` | Status | Implementation | Notes |
|--------|--------|----------------|-------|
| `auth` | **Implemented** | [`handler.go:authenticateClient`](../../internal/ws/handler.go) | The only client->server frame the server accepts today. |
| `subscribe` | **PLANNED** | n/a | Reserved. See §10 follow-up. |
| `unsubscribe` | **PLANNED** | n/a | Reserved. See §10 follow-up. |
| `ping` | **PLANNED** | n/a | Reserved. RFC 6455 PING/PONG handled transparently by the underlying library today. |

**Critical fact:** After auth completes, the server's `readPump` ([`internal/ws/client.go`](../../internal/ws/client.go) lines 74-90) **explicitly ignores** all incoming client frames. The read loop exists only to detect socket disconnect:

```go
// Post-auth messages are ignored; the read is only to detect
// disconnects and keep the connection alive.
```

This means SDK consumers MUST NOT today rely on subscribe/unsubscribe/ping wire frames -- the server will silently drop them. The TypeScript and Swift SDKs MUST gate any such send sites behind a feature flag tied to a future server version.

### 5.1 `auth`

> **Schema:** [`schemas/ws-messages.schema.json#/$defs/AuthPayload`](schemas/ws-messages.schema.json)
> **Fixture:** [`fixtures/websocket/auth.json`](fixtures/websocket/auth.json) (TODO)

```jsonc
{
  "type": "auth",
  "payload": {
    "token": "<opaque session token>"
  }
}
```

| Field | Classification | Notes |
|-------|----------------|-------|
| `token` | **P1** | Never log. See [`data-classification.md`](data-classification.md) §1.2. |

See §2.2 for full handshake semantics.

### 5.2 `subscribe` (PLANNED)

> **Schema:** [`schemas/ws-messages.schema.json#/$defs/SubscribePayload`](schemas/ws-messages.schema.json)

NOT yet implemented. Today, the server implicitly subscribes the client to ALL vehicles owned by the authenticated user as part of the auth handshake. A future revision will let the client narrow the subscription per-vehicle and pass a `sinceSeq` for snapshot-resume per NFR-3.11.

```jsonc
{
  "type": "subscribe",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "sinceSeq": 4271
  }
}
```

When implemented, the server response will be either:

- A normal `vehicle_update` stream starting at `sinceSeq + 1`, OR
- An `error` frame with `code: snapshot_required` indicating the client must perform a full REST snapshot fetch (NFR-3.11) and reconnect.

### 5.3 `unsubscribe` (PLANNED)

NOT yet implemented. Will release a per-vehicle subscription without closing the entire WebSocket.

```jsonc
{
  "type": "unsubscribe",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef"
  }
}
```

### 5.4 `ping` (PLANNED)

NOT yet implemented. Today, application-level ping is unnecessary because:

1. The underlying [`coder/websocket`](https://github.com/coder/websocket) library handles RFC 6455 PING/PONG control frames transparently in both directions.
2. The server emits `heartbeat` frames at a 15-second cadence (§7.4), giving the SDK a frequent positive liveness signal.

A future application-level `ping` is reserved for platforms where the WebSocket library does not expose RFC 6455 PING/PONG (some React Native runtimes, watchOS background sessions per NFR-3.36).

```jsonc
{
  "type": "ping",
  "payload": {
    "nonce": "<opaque round-trip ID>"
  }
}
```

The server response will be a `pong` echoing the nonce.

---

## 6. Errors and close codes

> **Anchored:** FR-7.1, FR-7.3, NFR-3.10, NFR-3.21.

### 6.1 Error frame

> **Schema:** [`schemas/ws-messages.schema.json#/$defs/ErrorPayload`](schemas/ws-messages.schema.json)
> **Fixture:** [`fixtures/websocket/error.auth_failed.json`](fixtures/websocket/error.auth_failed.json) (TODO), [`fixtures/websocket/error.auth_timeout.json`](fixtures/websocket/error.auth_timeout.json) (TODO)

```jsonc
{
  "type": "error",
  "payload": {
    "code": "auth_failed",
    "message": "invalid token"
  }
}
```

| Field | Type | Classification |
|-------|------|----------------|
| `code` | `string` (enum) | **P0** |
| `message` | `string` | **P0** (MUST NOT contain P1 values; see [`data-classification.md`](data-classification.md) §2.2) |

Per FR-7.1, consumer SDKs map `code` to typed error values and branch on the typed value, NEVER on the human-readable `message`. The `message` is intended for logs and developer tooling only.

### 6.1.1 Error code catalog

| Code | Today | Direction | Reconnect policy | Description |
|------|-------|-----------|------------------|-------------|
| `auth_failed` | **Implemented** ([`messages.go`](../../internal/ws/messages.go) `errCodeAuthFailed`) | server->client (handshake) | Surface to UI; require user to re-auth. **Do not auto-retry.** | Token signature/issuer/audience/expiry check failed, or `GetUserVehicles` failed. |
| `auth_timeout` | **Implemented** (`errCodeAuthTimeout`) | server->client (handshake) | Auto-retry with backoff (NFR-3.10) | Client did not send the auth frame within `HandlerConfig.AuthTimeout` (default 5s). |
| `permission_denied` | **PLANNED** | server->client | Surface to UI; do not auto-retry | Authenticated user attempted to subscribe to a vehicle they do not own. Today this is enforced silently by `Hub.Broadcast` filtering. |
| `vehicle_not_owned` | **PLANNED** | server->client | Surface to UI; do not auto-retry | Specific case of `permission_denied` for explicit subscribe (§5.2). |
| `rate_limited` | **PLANNED** | server->client / HTTP 429 | Auto-retry with backoff | Per-IP cap exceeded (currently surfaced as HTTP 429 on upgrade, not as an in-band frame). |
| `internal_error` | **PLANNED** | server->client | Auto-retry with backoff | Catch-all for unexpected server failures during a live session. |

The PLANNED codes are reserved in the AsyncAPI spec / message schema today so SDKs can match against them once the server emits them.

### 6.2 WebSocket close codes

> **Anchored:** RFC 6455 §7.4. Application-specific codes use the 4000-4999 range.

The Go server uses [`coder/websocket`](https://github.com/coder/websocket) status constants. Today the server explicitly closes the socket with the following codes:

| Code | Name | Today | Source (Go) | When | SDK reconnect policy |
|------|------|-------|-------------|------|----------------------|
| `1001` | Going Away | **Implemented** | [`client.go:writePump`](../../internal/ws/client.go) line 56 (`websocket.StatusGoingAway`) | Server is shutting down; hub closed the client's send channel | Auto-reconnect with backoff |
| `1008` | Policy Violation | **Implemented** | [`handler.go:handleUpgrade`](../../internal/ws/handler.go) line 93 (`websocket.StatusPolicyViolation`) | Authentication failed (sent immediately after the `error` frame) | Surface to UI; do NOT auto-retry on `auth_failed`. For `auth_timeout`, treat as transient and retry. |
| `1000` | Normal Closure | Tolerated | n/a (never explicitly emitted by server today; client-initiated only) | Client closed the socket cleanly | n/a (client-initiated) |

In addition, RFC 6455 reserves codes 4000-4999 for **application-specific** usage. The following application-specific codes are **PLANNED** (reserved by this contract for future server emission). SDKs SHOULD recognize them but MUST NOT panic on receipt of any 4xxx code they don't know.

| Code | Name | When | SDK reconnect policy |
|------|------|------|----------------------|
| `4001` | Auth Token Expired | Server detected mid-session token expiry (e.g., JWT `exp` passed) | Refresh token via `getToken()`, reconnect |
| `4002` | Permission Revoked | Vehicle ownership revoked while connected (e.g., invite removed) | Surface to UI, do not auto-retry the same vehicle |
| `4003` | Server Overload | Per-vehicle or per-user backpressure cap exceeded | Auto-reconnect with extended backoff |
| `4004` | Protocol Violation | Client sent a malformed frame or violated the atomic-group contract | Surface to UI as a bug; do not auto-retry |
| `4005` | Snapshot Required | Server cannot satisfy the requested `subscribe.sinceSeq` (gap too large) -- client must re-fetch the REST snapshot | Run the standard reconnect sequence (§7.2) |

The mapping between `error.payload.code` and the close code, when both are emitted, is:

| `error.code` | Following close code |
|--------------|---------------------|
| `auth_failed` | `1008` Policy Violation (today) |
| `auth_timeout` | `1008` Policy Violation (today) |
| `permission_denied` | `4002` Permission Revoked (PLANNED) |
| `vehicle_not_owned` | `4002` Permission Revoked (PLANNED) |
| `rate_limited` | `4003` Server Overload (PLANNED) |
| `internal_error` | `1011` Internal Error (PLANNED) |

### 6.3 No P1 in error messages

Per [`data-classification.md`](data-classification.md) §2.2 and Rule CG-DC-2: the `error.payload.message` field is P0, but error message construction sites MUST NOT include P1 values (no GPS, no addresses, no tokens, no email, no full VINs). Use opaque IDs (vehicle ID, drive ID, user ID) for correlation, or `redactVIN()` for VINs that absolutely must appear. The current implementation in [`handler.go:sendError`](../../internal/ws/handler.go) only sends static strings (`"invalid token"`, `"failed to load vehicles"`) and is therefore compliant.

---

## 7. Heartbeat, reconnect, and snapshot resume

> **Anchored:** NFR-3.10, NFR-3.11, NFR-3.12, NFR-3.13, FR-8.1.

### 7.1 Reconnect backoff parameters (NFR-3.10)

These are the canonical values from [`state-machine.md`](state-machine.md) §1.4 and MUST be implemented identically in both SDKs:

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Initial delay | 1 second | First reconnect attempt after disconnect |
| Backoff multiplier | 2x | Each subsequent attempt doubles the delay |
| Maximum delay | 30 seconds | Cap regardless of attempt count |
| Jitter | +/- 25% of computed delay | Prevents thundering herd at scale (NFR-3.6 5K concurrent clients) |
| Maximum retries | Unlimited (default) | SDK retries indefinitely unless `USER_STOPPED` or consumer configures a limit |

```
delay = min(initialDelay * 2^(attempt - 1), maxDelay)
jitter = delay * random(-0.25, +0.25)
effectiveDelay = delay + jitter
```

### 7.2 Reconnect sequence

The full reconnect sequence is documented end-to-end in [`state-machine.md`](state-machine.md) §5 (sequence diagram + reconnect invariants). The protocol-relevant invariants are:

1. **Snapshot before stream (NFR-3.11).** On reconnect, the SDK MUST re-fetch the REST snapshot ([`rest-api.md`](rest-api.md) `GET /vehicles/{id}/snapshot`) BEFORE processing any new WebSocket frames. The snapshot is the cold-load source of truth; the WebSocket stream resumes from the consistent baseline.
2. **All groups -> loading.** When the SDK begins the reconnect, every `dataState` group transitions to `loading` (see [`state-machine.md`](state-machine.md) §4.2 D-7). Cached values remain visible per NFR-3.12 / NFR-3.13.
3. **No forced reload (NFR-3.12).** The reconnect is entirely SDK-internal. The UI is never asked to refresh.
4. **Ordering guarantee.** Live frames received during the snapshot fetch are queued and applied AFTER the snapshot, NEVER before.
5. **Idempotent.** Multiple rapid disconnect/reconnect cycles MUST NOT cause duplicate snapshot fetches. The SDK cancels any in-flight fetch when a new reconnect begins.

The full reconnect handshake replays §2 verbatim: open WSS, send `auth` frame, await live frames.

### 7.3 Snapshot-resume semantics (NFR-3.11)

NFR-3.11 says: "On reconnect, SDK MUST re-fetch the DB snapshot and resume live stream without user intervention."

There are two valid implementations of this requirement, and the contract supports both:

1. **REST-snapshot resume (current v1.0 implementation).** Reconnect always fetches the full REST snapshot. No wire-level sequence numbers needed. Trade-off: extra HTTP round-trip on every reconnect, and gaps within a single connection are invisible.
2. **Sequence-resume (PLANNED, v1.x).** When the server begins emitting envelope `seq` (§3.3), the client passes its highest-seen `seq` as `subscribe.sinceSeq` (§5.2). The server replays missed frames OR responds with `error.code: snapshot_required` to fall back to mode 1. Trade-off: requires server-side per-connection retention of recent frames.

The SDK contract today is mode 1. The contract reserves the wire shape for mode 2 so v1.x can ship without a breaking change.

### 7.4 Heartbeat / keepalive

> **Anchored:** NFR-3.10 (reconnect cadence).

| Direction | Cadence | Wire form | Source (Go) |
|-----------|---------|-----------|-------------|
| Server -> client | Default 15 seconds (configurable via `WebSocketConfig.HeartbeatInterval`) | Bare envelope `{"type":"heartbeat"}` (no payload) | [`heartbeat.go:RunHeartbeat`](../../internal/ws/heartbeat.go) |
| Client -> server | None (today; PLANNED `ping` per §5.4) | n/a | n/a |
| Transport-level (RFC 6455 PING/PONG) | Handled transparently by `coder/websocket` | Binary control frames | Library internals |

The server pre-marshals the heartbeat message once at init (`heartbeatMessage = mustMarshal(...)`) and broadcasts it via [`Hub.BroadcastAll`](../../internal/ws/hub.go) line 90 to ALL connected clients regardless of vehicle ownership.

#### 7.4.1 SDK liveness watchdog

The SDK uses the heartbeat as a positive liveness signal:

- Reset a watchdog timer on every received frame (heartbeat, vehicle_update, anything).
- If the watchdog fires (no frame for `2 * heartbeatInterval`, default 30s), the SDK treats it as a silent disconnect and triggers `WS_CLOSED` -> `connecting` (state-machine.md C-9).

Per Rule CG-SM-1 ([`state-machine.md`](state-machine.md) §7), the watchdog MUST NOT be used to mark dataState `stale`. dataState transitions to `stale` only when the WebSocket actually closes (NFR-3.7, NFR-3.8b).

#### 7.4.2 SDK MUST NOT use heartbeat for freshness

The heartbeat is purely a liveness signal. Per NFR-3.7, freshness is event-driven and not time-based. The SDK MUST NOT:

- Mark fields stale because no `vehicle_update` for that field arrived in the last N heartbeats.
- Use heartbeat cadence to derive any data-state transition.

The only legitimate uses of heartbeat in the SDK are: (a) reset the liveness watchdog, (b) update an internal "last frame received" timestamp for debug telemetry.

---

## 8. Cross-references

| Topic | Document |
|-------|----------|
| Atomic group definitions and consistency predicates | [`vehicle-state-schema.md`](vehicle-state-schema.md) §2, §3 |
| Per-field types, units, classification, schemas | [`vehicle-state-schema.md`](vehicle-state-schema.md) §1 + [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json) |
| Data classification (P0/P1/P2) | [`data-classification.md`](data-classification.md) |
| Data lifecycle (DB vs WebSocket source-of-truth, retention) | [`data-lifecycle.md`](data-lifecycle.md) |
| connectionState / dataState / drive lifecycle state machines | [`state-machine.md`](state-machine.md) |
| Reconnect sequence diagram | [`state-machine.md`](state-machine.md) §5 |
| REST snapshot endpoint | [`rest-api.md`](rest-api.md) |
| AsyncAPI 3.0 spec (machine-readable) | [`specs/websocket.asyncapi.yaml`](specs/websocket.asyncapi.yaml) |
| Envelope JSON Schema | [`schemas/ws-envelope.schema.json`](schemas/ws-envelope.schema.json) |
| Per-message JSON Schemas | [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) |
| Server implementation entry points | [`internal/ws/handler.go`](../../internal/ws/handler.go), [`internal/ws/broadcaster.go`](../../internal/ws/broadcaster.go), [`internal/ws/nav_accumulator.go`](../../internal/ws/nav_accumulator.go), [`internal/ws/route_accumulator.go`](../../internal/ws/route_accumulator.go), [`internal/ws/heartbeat.go`](../../internal/ws/heartbeat.go) |
| Functional / non-functional requirements | [`docs/architecture/requirements.md`](../architecture/requirements.md) |

---

## 9. Type generation targets

### 9.1 TypeScript (AsyncAPI -> TS types)

The `gen-ts-ws-types` Makefile target (PLANNED) will invoke an AsyncAPI -> TypeScript generator against [`specs/websocket.asyncapi.yaml`](specs/websocket.asyncapi.yaml) and write the result to `sdk/typescript/src/types/ws-messages.ts`. The generator MUST consume the linked JSON Schemas via `$ref` rather than inlining. Drift between the generated file and the spec fails CI.

### 9.2 Swift (AsyncAPI -> Codable structs)

Per NFR-3.34, the Swift SDK uses `Codable`/`Sendable` structs. A code generator (PLANNED) will produce one struct per `$defs` entry in [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) plus the envelope from [`schemas/ws-envelope.schema.json`](schemas/ws-envelope.schema.json). The discriminator is implemented as an enum-with-associated-values per Swift idiom.

---

## 10. Open questions and follow-ups

These items are documented gaps between the protocol spec and the current `internal/ws/` implementation. They are non-blocking for MYR-11 (the spec captures the target shape and reserves the wire surface) but MUST be tracked as Linear follow-ups.

| # | Topic | Current behavior | Target | Proposed Linear issue title |
|---|-------|------------------|--------|------------------------------|
| 1 | **Envelope `seq` field (NFR-3.11)** | Server does not emit a per-connection sequence number. `wsMessage` struct has no `seq` field. | Server adds a `nextSeq` counter to `Client`, increments on every `Hub.Broadcast`/`BroadcastAll`, and includes it in every envelope. SDK uses it for gap detection and `subscribe.sinceSeq`. | `MYR-XX Add monotonic per-connection seq to WebSocket envelope (NFR-3.11)` |
| 2 | **Envelope-level `ts` field** | Server sets `payload.timestamp` on each payload type but no envelope-level `ts`. | Add envelope `ts` (server-authoritative ISO 8601 UTC) to disambiguate from any payload-internal timestamps and make ordering rules trivial. | `MYR-XX Add envelope-level ts to WebSocket frames` |
| 3 | **Client `subscribe`/`unsubscribe` frames** | `readPump` ignores all post-auth client frames. The client is implicitly subscribed to all owned vehicles at handshake time. | Implement `subscribe`/`unsubscribe` per §5.2-5.3 to enable per-vehicle routing and `sinceSeq` snapshot resume. | `MYR-XX Implement per-vehicle subscribe/unsubscribe on WebSocket` |
| 4 | **Application-level `ping`/`pong`** | Relies on RFC 6455 PING/PONG handled by `coder/websocket`. | Add app-level `ping`/`pong` for platforms where the WebSocket library does not expose RFC 6455 control frames (some RN runtimes, watchOS). | `MYR-XX Add application-level ping/pong frames` |
| 5 | **App-specific close codes (4001-4005)** | Server uses only `1001` (going away) and `1008` (policy violation). | Adopt the 4001-4005 catalog in §6.2 so the SDK can route reconnect/error UX precisely. | `MYR-XX Emit 4xxx WebSocket close codes for typed app failures` |
| 6 | **Nav debounce 200ms vs 500ms** | `defaultNavFlushInterval = 500ms`. NFR-3.2 specifies 200ms. Documented divergence in [`vehicle-state-schema.md`](vehicle-state-schema.md) §2.1. | EITHER tighten to 200ms (and accept partial-batch risk) OR amend NFR-3.2 to 500ms. Architect decision required. | `MYR-XX Reconcile nav debounce window: NFR-3.2 (200ms) vs implementation (500ms)` |
| 7 | **Vehicle ownership refresh mid-connection** | `Client.vehicleIDs` is a snapshot taken at handshake time. Revoking an invite while a viewer is connected does not stop the viewer's stream until they reconnect. | Add a hub-side ownership refresh hook (server-pushed) OR force-disconnect affected clients with close code 4002. | `MYR-XX Refresh WebSocket client vehicle scope on invite revocation` |
| 8 | **Auth success acknowledgement** | Server sends nothing on successful auth -- success is implicit in the next frame received. | Optionally send an `auth_ok` frame echoing `userID`, vehicle count, and (when seq lands) initial `seq`. | `MYR-XX Add auth_ok acknowledgement frame to WebSocket handshake` |
| 9 | **Fixture files for every message type** | Fixtures README lists no concrete files yet. The `fixtures/websocket/*.json` paths in this doc are forward references. | Author one happy-path fixture and at least one edge-case fixture per `type` listed in §4 / §5. Wire them into `contract-tester`. | `MYR-XX Author canonical WebSocket fixtures for every message type` |

---

## Change log

| Date | Change | Author |
|------|--------|--------|
| 2026-04-13 | Initial full draft replacing 28-line stub: handshake, envelope, server->client and client->server catalogs, error/close-code matrix, heartbeat/reconnect/snapshot semantics, AsyncAPI 3.0 spec at `specs/websocket.asyncapi.yaml`, sibling `ws-envelope.schema.json` and `ws-messages.schema.json`, follow-ups for envelope seq/ts, subscribe/unsubscribe/ping, 4xxx close codes, nav debounce reconciliation, ownership refresh, fixture authoring. | sdk-architect agent |
