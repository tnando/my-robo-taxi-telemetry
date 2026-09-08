# ops CLI

`cmd/ops` is the developer CLI for Tesla Fleet API operations and raw telemetry inspection. It replaces the deploy-and-`fly logs`-grep loop used for field verification work (MYR-25/28/29 and similar) and is the interim UX for the future shadcn/ui web test bench.

## Install

```bash
go install ./cmd/ops
# or
go build -o ./bin/ops ./cmd/ops
```

`go install` puts it on your `PATH` as `ops`. The examples below assume either that or `./bin/ops`.

## Environment variables

| Variable | Required for | Notes |
|---|---|---|
| `DATABASE_URL` | every subcommand | Supabase Postgres connection string. PgBouncer mode (`:6543`) is auto-detected. |
| `AUTH_TESLA_ID` | `auth token`, `auth link`, `fleet-config push` | Tesla OAuth client id. |
| `AUTH_TESLA_SECRET` | same as above | Tesla OAuth client secret. |
| `TESLA_PROXY_URL` | `fleet-config push` | Base URL of the running `tesla-http-proxy` sidecar (e.g. `https://localhost:4443`). |
| `OPS_OPERATOR` | every command that decrypts user data, including `fleet-config push` and `--all-streaming` | Your operator handle (e.g. `thomas`). No default; an email address is rejected. Written to an `AuditLog` `operator_decrypt` row **before** the decrypt (MYR-447). |
| `FLEET_TELEMETRY_HOSTNAME` | `fleet-config push` | Hostname vehicles connect to after config (e.g. `telemetry.myrobotaxi.app`). |
| `FLEET_TELEMETRY_PORT` | `fleet-config push` | Default `443`. |
| `FLEET_TELEMETRY_CA` | `fleet-config push` (prod) | PEM-encoded CA cert served with the telemetry endpoint. |
| `DEBUG_FIELDS_TOKEN` | `fields watch` | Shared secret for `/api/debug/fields`. Set identically on the server. In non-dev mode the server requires ≥32 chars and clients must present the token; under `--dev` it is optional. |

`.env.local` from the sibling Next.js app (`../react-frontend/.env.local`) contains every secret you need except `DEBUG_FIELDS_TOKEN`. The fastest local setup:

```bash
set -a && source ../react-frontend/.env.local && set +a
```

## Subcommands

Run `ops help` any time for the flag summary. Every subcommand prints JSON to stdout; progress/warning logs go to stderr so you can pipe through `jq`.

### `ops auth link --user-id <id> [--port 8765]`

Runs the full Tesla OAuth browser flow and writes fresh `access_token` + `refresh_token` to the DB. Use this when `ops auth token` fails with `401 login_required` (meaning the stored refresh_token has been revoked or expired — Tesla rotates aggressively).

**One-time setup on Tesla Developer portal:** add `http://localhost:8765/callback` to your Fleet API app's allowed redirect URIs. Tesla apps support multiple redirect URIs, so this sits next to your production web redirect with no conflict.

```bash
ops auth link --user-id clxy...
```

The CLI opens Tesla's login page in your browser, you approve the scopes, Tesla redirects back to `localhost:8765/callback`, the CLI swaps the code for tokens and persists them. Then:

```bash
ops auth token --user-id clxy...   # should now succeed
```

Flags:

- `--port` — local HTTP port the CLI listens on. Default `8765`. Must match the redirect URI registered on the Tesla app.
- `--scopes` — space-separated OAuth scopes. Default includes `openid`, `offline_access`, `vehicle_device_data`, `vehicle_cmds`, `vehicle_charging_cmds`.
- `--timeout` — how long to wait for the browser flow. Default `2m`.

PKCE (S256) is implemented per RFC 7636 — no client secret is sent in the authorize URL, and the code exchange is bound to a fresh verifier per flow.

### `ops auth token --user-id <id>`

Reads the user's Tesla OAuth token from the DB (`Account` table) and refreshes it if it will expire within one minute. Prints the access token, refresh token, and expiry:

```bash
ops auth token --user-id clxy... | jq
```

```json
{
  "userId": "clxy...",
  "accessToken": "eyJhbGciOi...",
  "refreshToken": "eyJhbGciOi...",
  "expiresAt": "2026-04-20T05:14:22Z",
  "refreshed": true
}
```

`refreshed: true` means the token was expired and was refreshed against `https://auth.tesla.com/oauth2/v3/token`. If `AUTH_TESLA_ID`/`AUTH_TESLA_SECRET` are not set, the command returns the existing (possibly expired) token with `refreshed: false` and a warning on stderr.

### `ops vehicles list --user-id <id>`

Lists every vehicle owned by the user:

```bash
ops vehicles list --user-id clxy... | jq
```

```json
[
  {
    "id": "clvx...",
    "vin": "5YJ3E7EB2NF000001",
    "name": "Red Taxi",
    "status": "parked",
    "chargeLevel": 78,
    "lastUpdated": "2026-04-20T04:12:33Z"
  }
]
```

Use this to grab the VIN and vehicle id before running any VIN-specific command below.

### `ops fleet-config show`

Prints the `DefaultFieldConfig` the server pushes to Tesla:

```bash
ops fleet-config show | jq '.TimeToFullCharge, .Location'
```

Useful for confirming a field + interval before pushing it.

### `ops fleet-config push --vin <vin> --user-id <id>`

Pushes `DefaultFieldConfig` to Tesla for one vehicle, via the `tesla-http-proxy`. Behavior mirrors the server's `POST /api/fleet-config/{vin}` endpoint (ownership check, auto-refresh, config exp set to 350 days):

```bash
ops fleet-config push --vin 5YJ3E7EB2NF000001 --user-id clxy... | jq
```

```json
{
  "vin": "5YJ3E7EB2NF000001",
  "userId": "clxy...",
  "tokenRefreshed": false,
  "updatedVehicles": 1,
  "skippedVehicles": null
}
```

If `skippedVehicles` is non-null, Tesla rejected the push — the map value explains why (common: `missing_key` means the vehicle has not been paired yet; run the virtual-key pairing flow).

### `ops fleet-config push --all-streaming [--apply] [--limit N]`

Re-pushes `DefaultFieldConfig` to **every already-streaming car** (MYR-630).

A fleet-telemetry config reaches a car exactly once, when it is pushed. Tesla stores no version and no hash, and nothing re-pushes a healthy car — the MYR-448 reconciler heals cars that have gone *quiet*, which is the precise complement of the set that matters here. So every change to `DefaultFieldConfig` (a new field, a new interval, MYR-629's `ResendIntervalSeconds` on `EnergyRemaining`) is **dormant for the whole existing fleet** until this sweep runs, and dormant silently: the cars keep streaming, they simply stream the old field set.

**Run it from the Fly machine, not a laptop.** The push has to reach the `tesla-http-proxy` that signs it, and that sidecar listens on loopback *inside* the container (`deployments/start.sh`). `ops` ships in the image next to `telemetry-server` for exactly this.

```bash
# 1. DRY RUN — pushes nothing. Read this first.
fly ssh console -a my-robo-taxi-telemetry -C "sh -lc 'OPS_OPERATOR=thomas ops fleet-config push --all-streaming'"

# 2. APPLY — actually pushes.
fly ssh console -a my-robo-taxi-telemetry -C "sh -lc 'OPS_OPERATOR=thomas ops fleet-config push --all-streaming --apply'"
```

`sh -lc` is required because `fly ssh console -C` execs the command directly rather than through a shell, so `OPS_OPERATOR=` could not be prefixed otherwise. `OPS_OPERATOR` is the **only** thing you have to supply — put your own handle there, since it is what the MYR-447 audit row records.

Everything else resolves on the machine without you:

- `DATABASE_URL`, `ENCRYPTION_KEY`, `AUTH_TESLA_ID`, `AUTH_TESLA_SECRET`, `FLEET_TELEMETRY_CA` are Fly secrets, already in the environment.
- `TESLA_PROXY_URL`, `FLEET_TELEMETRY_HOSTNAME` and `FLEET_TELEMETRY_PORT` are **not** environment variables in production — they live in `proxy.url`, `proxy.fleet_telemetry_hostname` and `proxy.fleet_telemetry_port` in `/etc/telemetry/config.json`, the file the server is started with. `ops` falls back to that file when the env does not carry them (`cmd/ops/fleet_config_file.go`), so the sweep reads the same values the server pushes with and the two cannot drift. Env still wins where it is set, so a laptop run that sources `../react-frontend/.env.local` is unchanged; `OPS_CONFIG_FILE` points the fallback at a different file.

**Dry run is the default.** Without `--apply` nothing is pushed; every car is still listed, its config is still read from Tesla, and the report says what would happen and why.

```json
{
  "mode": "dry-run",
  "limit": 50,
  "examined": 9,
  "pushed": 0,
  "wouldPush": 6,
  "skipped": 3,
  "failed": 0,
  "skipReasons": { "owner_suspended": 2, "missing_key": 1 },
  "vehicles": [
    {
      "vin": "5YJ3E7EB2NF000001",
      "userId": "clxy...",
      "vehicleName": "Model 3",
      "action": "would_push",
      "lastUpdated": "2026-09-08T14:02:11Z",
      "configAgeDays": 47.3
    }
  ]
}
```

Flags:

- `--apply` — perform the pushes. Default is the dry run.
- `--limit N` — cap the vehicles examined in one run. Default `50`. **Skips count against it**, so a run that spends its budget on suspended cars is a run whose report says so.
- `--vin` / `--user-id` are refused alongside `--all-streaming`, so the combination cannot read as "push this one VIN to the whole fleet".

**Config age.** There is no "pushed at" column anywhere. Every push in this codebase sets `exp` to exactly 350 days from the moment it was sent, so Tesla's echoed `exp` dates the push: `age = 350d - (exp - now)`. Tesla documents `synced` but not that it echoes `exp`, so a nil `exp` reads as *unknown age* (the field is simply absent) rather than as zero. That arithmetic is also the sweep's own verification — **re-run the dry run after an `--apply` and every pushed car reports a `configAgeDays` of roughly zero.**

**Skip reasons** (deliberate refusals; they do not fail the run):

| Reason | Meaning |
|---|---|
| `owner_suspended` | MYR-592 removed this car's config for owner inactivity. Re-pushing would silently reverse a cost decision; the owner's reconnect is the only thing that may. |
| `config_absent` | The last push did not take (`go_fleet_config_attempts`). Nothing to refresh — the MYR-448 reconciler owns the retry. |
| `awaiting_owner_ack` | Driver-linked car whose owner-approval acknowledgment is outstanding (MYR-599 consent-wins). |
| `no_token` | No Tesla token on file for the owner. Permanently unpushable, not transient. |
| `no_config` | Tesla reports nothing configured for the VIN. That is the reconciler's candidate, not this tool's. |
| `missing_key` | Tesla answered `200` and did nothing: the virtual key is not enrolled. |

**Failure reasons** (things that went wrong and may not next time): `token_failed`, `config_read_failed`, `push_failed`. A non-zero `failed` count exits non-zero, so a scripted run cannot read a fleet of failures as success. Skips do not.

**Two writes still happen on a dry run**, both deliberate:

- an **OAuth token refresh**, when a stored token has expired. Tesla's refresh tokens are single-use, so not persisting the new pair would break the owner's next server-side call. Serialized through the account row lock (MYR-595, `RotateTeslaTokenLockedWaiting`) — never the unguarded on-demand path, because this sweep walks many owners' tokens while the live server may be refreshing the same accounts.
- the **MYR-447 operator-decrypt audit row**, one per owner whose token is read (not one per car). A failure to write it aborts the run.

**Idempotent.** A push *replaces* a car's config, so running the sweep twice leaves the fleet as running it once did. The sweep writes nothing of its own to our database — no attempt rows, no schedule — so nothing accumulates and no run has to be completed or rolled back. A capped run is not resumable in the sense of picking up where it stopped; it is re-runnable, which is stronger.

Cars are examined **most recently active first**, so when a cap truncates the run the cars reached first are the ones actually streaming.

### `ops fields snapshot --vin <vin>`

One-shot dump of the current `Vehicle` row as JSON — the values the Next.js app reads from the DB:

```bash
ops fields snapshot --vin 5YJ3E7EB2NF000001 | jq
```

Use this to confirm a persisted value (e.g. `destinationName` or `tripDistanceRemaining`) matches what the UI is showing, independent of whether the WebSocket is connected.

### `ops fields watch --vin <vin> [--server <url>] [--token <token>]`

Streams raw decoded protobuf fields from the server's `/api/debug/fields` WebSocket endpoint, one JSON frame per decoded Tesla payload. Every frame contains every field the vehicle sent — including fields the broadcast pipeline filters out — with Tesla proto field numbers preserved:

```bash
ops fields watch --vin 5YJ3E7EB2NF000001 --server ws://localhost:8080 | jq
```

```json
{
  "vin": "5YJ3E7EB2NF000001",
  "timestamp": "2026-04-20T04:15:00.123Z",
  "fields": {
    "TimeToFullCharge": { "value": 1.5, "protoField": 43, "type": "double" },
    "Soc":              { "value": 78.2, "protoField": 8,  "type": "double" },
    "Location":         { "value": { "Latitude": 37.77, "Longitude": -122.41 }, "protoField": 21, "type": "location" },
    "Odometer":         { "value": null, "protoField": 5, "type": "invalid", "invalid": true }
  }
}
```

- `--server` accepts `ws://`, `wss://`, `http://`, or `https://`. `http*` is auto-upgraded to `ws*`.
- Omit `--vin` to stream all vehicles (useful when inspecting a fleet).
- Auth: if the server has `DEBUG_FIELDS_TOKEN` set, pass the same value via `--token` or the env var. The CLI always uses the `X-Debug-Token` header (query-param form exists for browsers but shows up in access logs).

#### How the endpoint gets mounted

`/api/debug/fields` is mounted under **either** gate:

| Gate | How to enable | Token required from client? | Intended use |
|---|---|---|---|
| **`--dev`** | `go run ./cmd/telemetry-server --dev --config configs/dev-notls.json` | No (but honored if set) | Local laptop server + simulator |
| **`DEBUG_FIELDS_TOKEN`** | Set on the server (any mode). **In non-dev mode the token must be ≥32 chars** or startup fails. | Yes — client must present the same token | Production server, tailing real-Tesla frames |

Startup logs print which gate is active and whether the token is required. If neither gate is satisfied, the endpoint is not mounted and raw field publication is off (zero cost).

#### Streaming against production (the headline workflow)

Real Teslas only dial the production server, so that's where `ops fields watch` has to connect for actual field verification. Enable the endpoint on Fly once:

```bash
flyctl secrets set DEBUG_FIELDS_TOKEN="$(openssl rand -base64 32)" -a myrobotaxi-telemetry
# Fly redeploys. Save the token locally — you cannot read it back from Fly.
```

Then from your laptop:

```bash
export DEBUG_FIELDS_TOKEN='<the value you just generated>'
ops fields watch --vin 7SAYGDET7TA613795 --server wss://telemetry.myrobotaxi.app
```

Drive the car (or wake it up) and frames stream into your terminal in real time. Rotate the secret by re-running `flyctl secrets set` with a new value.

#### Streaming against a local server

For the simulator or local development, use `--dev` so no token dance is needed:

```bash
go run ./cmd/telemetry-server --dev --config configs/dev-notls.json
ops fields watch --vin <vin> --server ws://localhost:8080
```

`configs/dev-notls.json` leaves TLS empty so you don't need to generate local certs just to boot the server.

## End-to-end recipe: verifying a Tesla field empirically (MYR-25 style)

The workflow that motivated this tool. Example: confirm the units of `TimeToFullCharge`.

One-time setup — set the debug token on the production server:

```bash
flyctl secrets set DEBUG_FIELDS_TOKEN="$(openssl rand -base64 32)" -a myrobotaxi-telemetry
# Copy the value into a secret manager / password manager for reuse.
```

Then per verification session:

```bash
# 1. Confirm the fleet is asking Tesla to send the field at a reasonable interval.
ops fleet-config show | jq '.TimeToFullCharge'
ops fleet-config push --vin 5YJ3... --user-id clxy...   # only if the interval changed

# 2. Tail the live stream from production while the car is awake/driving/charging.
export DEBUG_FIELDS_TOKEN='<the value from the Fly secret>'
ops fields watch --vin 5YJ3... --server wss://telemetry.myrobotaxi.app \
  | jq -c 'select(.fields.TimeToFullCharge) | {t: .timestamp, v: .fields.TimeToFullCharge}'
```

Watch a few frames, compare against the in-car display, and you can conclude whether the field arrives in hours, minutes, seconds, etc. — no redeploy, no Fly log tail.

> **Why production, not local?** Real Teslas only dial the server hostname pinned in their on-board `fleet_telemetry_config`. That pointer only ever resolves to the Fly-deployed prod server. A local `--dev` server is for the simulator; it will never receive a frame from a real car.

## Troubleshooting

- **`DATABASE_URL is required`** — source `../react-frontend/.env.local` (or set the var directly).
- **`vehicle owner mismatch`** on `fleet-config push` — the `userId` you passed does not own the VIN. Run `ops vehicles list --user-id <id>` to confirm.
- **Empty output from `fields watch`** — the vehicle isn't streaming to the server you connected to. Remember real Teslas only dial production; pointing `--server` at a local `--dev` server with no simulator running will always be silent. Check the server logs for a `vehicle connected` line, or confirm with `ops fields snapshot --vin <vin>` that `lastUpdated` is recent.
- **`/api/debug/fields` endpoint not found (404)** — the server was started without either gate. Pass `--dev` locally, or set `DEBUG_FIELDS_TOKEN` (≥32 chars) on the deployed server.
- **Server refuses to start with `DEBUG_FIELDS_TOKEN must be at least 32 chars`** — non-dev mode enforces a length floor so weak tokens can't reach prod. Generate a proper one: `openssl rand -base64 32`.
- **`unexpected client frame` debug logs on the server** — safe to ignore. The debug endpoint is server→client only; any frame the client sends is logged and discarded.
- **`unauthorized` on `fields watch`** — `DEBUG_FIELDS_TOKEN` on the server does not match `--token`/the env var. Both sides must agree (or both be empty).
- **`401 login_required` on `ops auth token`** — the stored `refresh_token` is dead. Run `ops auth link --user-id <id>` to refresh via the browser OAuth flow, then retry.
- **`listen on port 8765 ... address already in use` on `ops auth link`** — another process holds the port. Close it or pass `--port <free-port>` (and make sure that port is registered on the Tesla app's redirect URIs).
