package main

import (
	"flag"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/fleetrepush"
)

// --all-streaming and --vin ask for two different things, and the reading a
// hurried operator would put on the combination ("push this VIN everywhere") is
// not one the tool offers. Refuse rather than silently ignoring half of it.
func TestRejectSingleVINFlags(t *testing.T) {
	tests := []struct {
		name    string
		vin     string
		userID  string
		wantErr bool
	}{
		{name: "neither", wantErr: false},
		{name: "vin only", vin: "5YJ3E1EA1NF000801", wantErr: true},
		{name: "user only", userID: "clxy", wantErr: true},
		{name: "both", vin: "5YJ3E1EA1NF000801", userID: "clxy", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rejectSingleVINFlags(tt.vin, tt.userID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("rejectSingleVINFlags(%q, %q) = %v, wantErr %v", tt.vin, tt.userID, err, tt.wantErr)
			}
		})
	}
}

// THE FLAG DEFAULT IS THE SAFETY PROPERTY. `ops fleet-config push
// --all-streaming` with nothing else must be a dry run; if --apply ever
// defaulted true, one keystroke would push config to the whole fleet.
func TestRepushFlagDefaults(t *testing.T) {
	fs := newTestFlagSet()
	allStreaming, opts := registerRepushFlags(fs)
	if err := fs.Parse([]string{"--all-streaming"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*allStreaming {
		t.Fatal("--all-streaming did not set its flag")
	}
	if opts.apply {
		t.Fatal("apply defaults TRUE — the default run must push nothing")
	}
	if opts.limit != fleetrepush.DefaultLimit {
		t.Errorf("limit default = %d, want %d", opts.limit, fleetrepush.DefaultLimit)
	}
}

func TestRepushFlagsParseApplyAndLimit(t *testing.T) {
	fs := newTestFlagSet()
	allStreaming, opts := registerRepushFlags(fs)
	if err := fs.Parse([]string{"--all-streaming", "--apply", "--limit", "7"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*allStreaming || !opts.apply || opts.limit != 7 {
		t.Fatalf("got allStreaming=%v apply=%v limit=%d, want true/true/7",
			*allStreaming, opts.apply, opts.limit)
	}
}

// The sweep is only runnable from inside the app container: the tesla-http-proxy
// it pushes through listens on loopback there (deployments/start.sh), and the
// secrets it needs exist only as Fly app secrets. So the binary has to be in the
// image — a fact that lives in a Dockerfile nothing else tests, and whose
// absence shows up as `exec: "ops": executable file not found in $PATH` on a
// production shell, which is exactly how MYR-630 was discovered.
func TestDockerfileShipsOpsBinary(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(b)

	buildLine := regexp.MustCompile(`go build[^\n]*-o /ops \./cmd/ops`)
	if !buildLine.MatchString(dockerfile) {
		t.Error("no builder stage line compiling ./cmd/ops to /ops")
	}
	if !strings.Contains(dockerfile, "COPY --from=builder /ops /usr/local/bin/ops") {
		t.Error("the runtime stage does not copy /ops onto the PATH; " +
			"`fly ssh console -C \"ops ...\"` would fail with executable file not found")
	}
	// It must land in the SAME stage as the server, or it ships without the
	// runtime the server's own COPY lines establish.
	serverCopy := strings.Index(dockerfile, "COPY --from=builder /telemetry-server")
	opsCopy := strings.Index(dockerfile, "COPY --from=builder /ops ")
	if serverCopy < 0 || opsCopy < 0 || opsCopy < serverCopy {
		t.Errorf("ops is copied at %d, telemetry-server at %d — ops must ship alongside the server",
			opsCopy, serverCopy)
	}
}

// newTestFlagSet builds a flag set that reports parse errors instead of
// exiting, so a bad flag fails the test rather than the process.
func newTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("fleet-config push", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}
