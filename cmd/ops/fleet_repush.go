package main

// `ops fleet-config push --all-streaming` — the MYR-630 fleet-wide re-push.
//
// A change to DefaultFieldConfig reaches only cars that are pushed again, and
// nothing re-pushes a healthy car: the MYR-448 reconciler heals cars that have
// gone QUIET, which is the complement of the set that matters here. So MYR-629's
// EnergyRemaining resend — and every future field or interval change — stays
// dormant across the existing fleet until an operator runs this.
//
// The policy lives in internal/fleetrepush; this file is wiring, flags and the
// MYR-447 obligations that come with reading owners' Tesla credentials.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/myrobotaxi/telemetry/internal/fleetrepush"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// repushOptions are the flags `--all-streaming` reads. Grouped so
// runFleetConfigPush stays a dispatcher rather than growing a second body.
type repushOptions struct {
	apply bool
	limit int
}

// runFleetConfigRepush sweeps the whole streaming fleet.
//
// DRY RUN IS THE DEFAULT and the report is written to stdout either way. Two
// writes still happen without --apply — a single-use OAuth token refresh, and
// the operator-decrypt audit row — both explained in internal/fleetrepush/doc.go.
func runFleetConfigRepush(ctx context.Context, opts repushOptions) error {
	operator, err := requireOperator()
	if err != nil {
		return err
	}
	endpoint, err := loadEndpointConfig()
	if err != nil {
		return err
	}
	proxyURL := os.Getenv("TESLA_PROXY_URL")
	if proxyURL == "" {
		return fmt.Errorf("TESLA_PROXY_URL is required for fleet-config push")
	}

	logger := newLogger()
	db, err := openDB(ctx, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	accountRepo, err := newAccountRepo(db)
	if err != nil {
		return err
	}

	deps := buildRepushDeps(db, accountRepo, logger, operator, proxyURL, endpoint)

	report, runErr := fleetrepush.New(deps, fleetrepush.Config{
		Apply: opts.apply,
		Limit: opts.limit,
	}, logger).Run(ctx)

	// The report is printed even when the run aborted: a partial sweep has
	// already changed real cars and the operator must see which.
	if err := writeJSON(os.Stdout, report); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("fleet-config re-push sweep: %w", runErr)
	}
	// A failed push is not a failed command — the report names every one — but
	// the exit code has to say so, or a scripted run reads a fleet of failures
	// as success. Skips are deliberate refusals and do NOT fail the run.
	if report.Failed > 0 {
		return fmt.Errorf("%d of %d vehicles failed; see failureReasons above",
			report.Failed, report.Examined)
	}
	return nil
}

// buildRepushDeps wires the sweep's collaborators.
//
// TWO FLEET API CLIENTS, ON PURPOSE, exactly as the reconciler's wiring
// documents: the config READ is an unsigned authenticated call that must go to
// the direct Fleet API, and the PUSH must go through the tesla-http-proxy,
// which signs it. Sending either to the other's base URL fails.
func buildRepushDeps(
	db *store.DB,
	accountRepo *store.AccountRepo,
	logger *slog.Logger,
	operator, proxyURL string,
	endpoint telemetry.EndpointConfig,
) fleetrepush.Deps {
	reader := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL: os.Getenv("FLEET_API_BASE_URL"), // empty => default NA Fleet API
	}, logger.With(slog.String("subcomponent", "fleet-read")))

	writer := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL:    proxyURL,
		HTTPClient: proxyHTTPClient(proxyURL, logger),
	}, logger.With(slog.String("subcomponent", "fleet-push")))

	return fleetrepush.Deps{
		// No encryptor: this listing reads ids, VIN, name and timestamps and
		// decrypts nothing, so it is not an operator decrypt. The TOKEN read
		// below is, and that one is audited.
		Store:    &repushStore{repo: store.NewVehicleRepo(db.Pool(), store.NoopMetrics{})},
		Tokens:   newRepushTokenSource(accountRepo, logger),
		Reader:   &repushConfigReader{client: reader},
		Pusher:   &repushPusher{client: writer, endpoint: endpoint},
		Classify: repushSkipClassifier{},
		Auditor: &repushAuditor{
			auditor:  newOperatorAuditor(db),
			operator: operator,
		},
	}
}

// registerRepushFlags adds the sweep's flags to the `fleet-config push` flag
// set. Kept next to the sweep so the single-VIN path in fleet.go does not have
// to know about them.
func registerRepushFlags(fs *flag.FlagSet) (allStreaming *bool, opts *repushOptions) {
	opts = &repushOptions{}
	allStreaming = fs.Bool("all-streaming", false,
		"re-push DefaultFieldConfig to EVERY already-streaming car (MYR-630). "+
			"Dry run unless --apply is also given.")
	fs.BoolVar(&opts.apply, "apply", false,
		"with --all-streaming: actually push. Default is a dry run that pushes nothing.")
	fs.IntVar(&opts.limit, "limit", fleetrepush.DefaultLimit,
		"with --all-streaming: cap the vehicles examined in one run (skips count against it).")
	return allStreaming, opts
}

// rejectSingleVINFlags refuses the flag combination that would otherwise read
// as "push this one VIN to the whole fleet".
func rejectSingleVINFlags(vin, userID string) error {
	if vin != "" || userID != "" {
		return fmt.Errorf("--all-streaming sweeps the whole fleet; do not pass --vin or --user-id")
	}
	return nil
}
