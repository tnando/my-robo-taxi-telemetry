package main

// Reading the fleet-config push settings out of the SERVER'S OWN config file
// when the environment does not carry them (MYR-630).
//
// ── WHY THIS EXISTS ─────────────────────────────────────────────────────────
//
// `ops fleet-config push` was written for a laptop, where the operator sources
// ../react-frontend/.env.local and every setting arrives as an env var. On the
// Fly machine — the ONLY place the fleet-wide sweep can run, because the
// tesla-http-proxy it must reach listens on loopback inside that container —
// the same three settings are not in the environment at all. They live in
// /etc/telemetry/config.json, which the server is started with:
//
//	proxy.url                       → TESLA_PROXY_URL
//	proxy.fleet_telemetry_hostname  → FLEET_TELEMETRY_HOSTNAME
//	proxy.fleet_telemetry_port      → FLEET_TELEMETRY_PORT
//
// (FLEET_TELEMETRY_CA is a Fly secret and IS in the environment, so it has no
// file fallback here.) Without this, the documented `fly ssh console` command
// fails on its first line with "TESLA_PROXY_URL is required".
//
// ── ENV WINS, ALWAYS ────────────────────────────────────────────────────────
//
// The file is a FALLBACK, never an override: a laptop run with env set behaves
// exactly as it did before, and the file is not even opened unless something is
// missing. This is the same precedence the server uses — config.Load reads the
// file first and then overlays env — so the two cannot disagree about which
// source wins.
//
// ── WHY NOT config.Load ─────────────────────────────────────────────────────
//
// internal/config.Load would be the obvious reuse, and it is the wrong tool: it
// validates the WHOLE server config, including TLS cert paths that start.sh
// exports into the server's process and that an `fly ssh console` shell does
// not inherit. The ops CLI would then fail on requirements it has no use for.
// Parsing the one object it actually needs keeps the failure surface to the
// three fields being read.

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// defaultOpsConfigFile is where the Dockerfile puts the server's config, and
// what the CMD starts it with.
const defaultOpsConfigFile = "/etc/telemetry/config.json"

// opsConfigFileEnv overrides that path, for a laptop pointing at a checkout's
// configs/ directory.
const opsConfigFileEnv = "OPS_CONFIG_FILE"

// fleetConfigFileSettings is the sliver of the server config this CLI reads.
// Field names mirror internal/config's fileConfig JSON tags exactly; anything
// else in the file is ignored.
type fleetConfigFileSettings struct {
	Proxy struct {
		URL                    string `json:"url"`
		FleetTelemetryHostname string `json:"fleet_telemetry_hostname"`
		FleetTelemetryPort     int    `json:"fleet_telemetry_port"`
	} `json:"proxy"`
}

var (
	fleetConfigFileOnce sync.Once
	fleetConfigFileVal  fleetConfigFileSettings
)

// fleetConfigFromFile reads the server config once per process. A missing or
// unparseable file is NOT an error: it yields empty settings, and the caller
// then fails with the env-var message it would have printed anyway — which is
// the actionable one, because setting the env var is the fix in both cases.
func fleetConfigFromFile() fleetConfigFileSettings {
	fleetConfigFileOnce.Do(func() {
		fleetConfigFileVal = readFleetConfigFile(opsConfigFilePath())
	})
	return fleetConfigFileVal
}

// opsConfigFilePath is the file the fallback reads.
func opsConfigFilePath() string {
	if path := os.Getenv(opsConfigFileEnv); path != "" {
		return path
	}
	return defaultOpsConfigFile
}

// readFleetConfigFile parses one config file, tolerating every way it can be
// unavailable. Split out of the sync.Once so it is testable.
func readFleetConfigFile(path string) fleetConfigFileSettings {
	var out fleetConfigFileSettings
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, by design
	if err != nil {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fleetConfigFileSettings{}
	}
	return out
}

// resolveProxyURL returns the tesla-http-proxy base URL: env first, then the
// server's config file.
func resolveProxyURL() (string, error) {
	if v := os.Getenv("TESLA_PROXY_URL"); v != "" {
		return v, nil
	}
	if v := fleetConfigFromFile().Proxy.URL; v != "" {
		return v, nil
	}
	return "", fmt.Errorf(
		"TESLA_PROXY_URL is required for fleet-config push (and %s has no proxy.url)",
		defaultOpsConfigFile)
}
