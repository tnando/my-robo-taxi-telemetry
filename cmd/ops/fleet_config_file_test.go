package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The Fly machine carries these settings in the server's config file, not in
// the environment, so the sweep's documented `fly ssh console` command depends
// entirely on this parse finding them.
func TestReadFleetConfigFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("reads the proxy block the server is started with", func(t *testing.T) {
		path := filepath.Join(dir, "config.json")
		// Trimmed copy of configs/fly.json, extra keys included on purpose:
		// the parse must ignore everything it does not ask for.
		write(t, path, `{
		  "server": {"tesla_port": 8443},
		  "proxy": {
		    "url": "https://127.0.0.1:4443",
		    "fleet_telemetry_hostname": "telemetry.myrobotaxi.app",
		    "fleet_telemetry_port": 443
		  }
		}`)
		got := readFleetConfigFile(path)
		if got.Proxy.URL != "https://127.0.0.1:4443" {
			t.Errorf("URL = %q", got.Proxy.URL)
		}
		if got.Proxy.FleetTelemetryHostname != "telemetry.myrobotaxi.app" {
			t.Errorf("hostname = %q", got.Proxy.FleetTelemetryHostname)
		}
		if got.Proxy.FleetTelemetryPort != 443 {
			t.Errorf("port = %d", got.Proxy.FleetTelemetryPort)
		}
	})

	// Every unavailability is the same answer: empty. The caller then prints
	// the env-var error, which is the actionable message in all of these cases.
	for _, tc := range []struct{ name, body string }{
		{name: "no proxy block", body: `{"server":{"tesla_port":8443}}`},
		{name: "not json", body: `not json at all`},
		{name: "empty file", body: ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			write(t, path, tc.body)
			if got := readFleetConfigFile(path); got != (fleetConfigFileSettings{}) {
				t.Errorf("want zero settings, got %+v", got)
			}
		})
	}

	t.Run("missing file is not an error", func(t *testing.T) {
		if got := readFleetConfigFile(filepath.Join(dir, "absent.json")); got != (fleetConfigFileSettings{}) {
			t.Errorf("want zero settings, got %+v", got)
		}
	})
}

// Env wins so a laptop run, where the operator sources .env.local, behaves
// exactly as it did before the fallback existed.
func TestResolveProxyURLPrefersEnv(t *testing.T) {
	t.Setenv("TESLA_PROXY_URL", "https://localhost:9999")
	got, err := resolveProxyURL()
	if err != nil {
		t.Fatalf("resolveProxyURL: %v", err)
	}
	if got != "https://localhost:9999" {
		t.Errorf("got %q, want the env value", got)
	}
}

func TestOpsConfigFilePath(t *testing.T) {
	t.Setenv(opsConfigFileEnv, "")
	if got := opsConfigFilePath(); got != defaultOpsConfigFile {
		t.Errorf("got %q, want %q", got, defaultOpsConfigFile)
	}
	t.Setenv(opsConfigFileEnv, "/custom/config.json")
	if got := opsConfigFilePath(); got != "/custom/config.json" {
		t.Errorf("got %q, want the override", got)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
