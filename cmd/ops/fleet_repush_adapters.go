package main

// Boundary translations between the ops CLI's wiring and the MYR-630 sweep's
// consumer-site interfaces. Every type here is an adapter: no decision lives in
// this file. Split from fleet_repush.go for the 300-line cap, mirroring
// cmd/sweep-orphan-fleet-configs/adapters.go — which is also where the token
// adapters below come from, replicated rather than shared because that package
// is a main of its own.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/myrobotaxi/telemetry/internal/fleetrepush"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// repushStore adapts store.VehicleRepo to fleetrepush.Store.
type repushStore struct {
	repo *store.VehicleRepo
}

func (a *repushStore) ListStreamingFleetConfigVehicles(
	ctx context.Context, limit int,
) ([]fleetrepush.Candidate, error) {
	rows, err := a.repo.ListStreamingFleetConfigVehicles(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("list streaming fleet-config vehicles: %w", err)
	}
	out := make([]fleetrepush.Candidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, fleetrepush.Candidate{
			VehicleID:       r.VehicleID,
			VIN:             r.VIN,
			UserID:          r.UserID,
			VehicleName:     r.VehicleName,
			LastUpdated:     r.LastUpdated,
			Status:          r.Status,
			Suspended:       r.Suspended,
			ConfigAbsent:    r.ConfigAbsent,
			PendingOwnerAck: r.PendingOwnerAck,
		})
	}
	return out, nil
}

// repushConfigReader adapts the Fleet API client's config read, mapping a 404
// onto fleetrepush.ErrNoConfig so an absent config reads as a skip rather than
// a failure.
type repushConfigReader struct {
	client *telemetry.FleetAPIClient
}

func (a *repushConfigReader) GetTelemetryConfig(
	ctx context.Context, token, vin string,
) (fleetrepush.ConfigStatus, error) {
	res, err := a.client.GetTelemetryConfig(ctx, token, vin)
	if err != nil {
		var apiErr *telemetry.FleetAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return fleetrepush.ConfigStatus{}, fmt.Errorf("%w: 404", fleetrepush.ErrNoConfig)
		}
		return fleetrepush.ConfigStatus{}, err
	}
	if res == nil {
		return fleetrepush.ConfigStatus{}, fleetrepush.ErrNoConfig
	}
	st := fleetrepush.ConfigStatus{
		// EITHER signal counts: Tesla returns config:null when nothing is set,
		// and a non-nil config with synced=false is a pushed-but-unapplied
		// config, which is still a config on file worth refreshing.
		Configured: res.Response.Synced || res.Response.Config != nil,
	}
	if res.Response.Config != nil {
		st.Exp = res.Response.Config.Exp
	}
	return st, nil
}

// repushPusher sends DefaultFieldConfig through the signing proxy. It is the
// CLI's copy of realFleetPusher.PushForVIN, including the part that matters:
// SkipErrorFor turns Tesla's 200-with-skips into an error, so an unpaired car
// cannot be counted as pushed.
type repushPusher struct {
	client   *telemetry.FleetAPIClient
	endpoint telemetry.EndpointConfig
}

func (a *repushPusher) PushForVIN(ctx context.Context, token, vin string) error {
	exp := time.Now().Add(350 * 24 * time.Hour).Unix()
	var ca *string
	if a.endpoint.CA != "" {
		ca = &a.endpoint.CA
	}
	result, err := a.client.PushTelemetryConfig(ctx, token, telemetry.FleetConfigRequest{
		VINs: []string{vin},
		Config: telemetry.FleetConfig{
			Hostname:   a.endpoint.Hostname,
			Port:       a.endpoint.Port,
			CA:         ca,
			Fields:     telemetry.DefaultFieldConfig(),
			AlertTypes: []string{"service"},
			Exp:        &exp,
		},
	})
	if err != nil {
		return err
	}
	return telemetry.SkipErrorFor(result, vin)
}

// repushSkipClassifier keeps the one errors.As that would otherwise force
// internal/fleetrepush to import internal/telemetry.
type repushSkipClassifier struct{}

func (repushSkipClassifier) IsAwaitingVirtualKey(err error) bool {
	var skipped *telemetry.SkippedVehicleError
	return errors.As(err, &skipped) && skipped.AwaitingVirtualKey()
}

// repushAuditor writes the MYR-447 operator-decrypt row for one owner's Tesla
// credentials. TargetType is the USER, because the credential belongs to the
// account rather than to any one car — the same shape the single-VIN push
// writes for its token read.
type repushAuditor struct {
	auditor  *store.OperatorAuditor
	operator string
}

func (a *repushAuditor) RecordTokenDecrypt(ctx context.Context, userID string) error {
	err := a.auditor.RecordDecrypt(ctx, store.OperatorAccess{
		Operator:   a.operator,
		Command:    "ops fleet-config push --all-streaming",
		UserID:     userID,
		TargetType: store.OperatorTargetUser,
		TargetID:   userID,
		Fields:     teslaTokenAuditFields,
	})
	if err != nil {
		return fmt.Errorf("record operator decrypt: %w", err)
	}
	return nil
}

// newRepushTokenSource builds the SERIALIZED token path (MYR-595) — the same
// resolver the server runs, with the account row lock wired in. The CLI's own
// resolveTeslaToken (fleet.go) is the unguarded copy and is deliberately NOT
// reused here: this sweep walks many owners' tokens while the live server may
// be refreshing the same accounts, and Tesla's refresh tokens are single-use.
func newRepushTokenSource(repo *store.AccountRepo, logger *slog.Logger) *repushTokenSource {
	var opts []telemetry.TeslaTokenResolverOption
	if id := os.Getenv("AUTH_TESLA_ID"); id != "" {
		refresher := telemetry.NewTokenRefresher(telemetry.TeslaOAuthConfig{
			ClientID:     id,
			ClientSecret: os.Getenv("AUTH_TESLA_SECRET"),
		}, logger.With(slog.String("subcomponent", "token-refresh")))
		opts = append(opts,
			telemetry.WithResolverRefresher(refresher, &repushTokenUpdater{repo: repo}),
			telemetry.WithResolverRotator(&repushTokenRotator{repo: repo}),
		)
	} else {
		logger.Warn("AUTH_TESLA_ID is unset: an expired token cannot be refreshed, " +
			"so those vehicles will report failed rather than being re-pushed")
	}
	return &repushTokenSource{
		resolver: telemetry.NewTeslaTokenResolver(&repushTokenProvider{repo: repo},
			logger.With(slog.String("subcomponent", "token")), opts...),
	}
}

// repushTokenSource adapts the resolver, mapping ONLY the no-row sentinel onto
// ErrNoToken. An expired-but-unrefreshable token stays an ordinary error so it
// lands as `failed`: a refresh failure can be transient, whereas a missing
// account row is permanent, and folding them together would overstate the
// unreachable set.
type repushTokenSource struct {
	resolver *telemetry.TeslaTokenResolver
}

func (a *repushTokenSource) AccessToken(ctx context.Context, userID string) (string, error) {
	tok, err := a.resolver.Resolve(ctx, userID)
	switch {
	case errors.Is(err, telemetry.ErrTeslaTokenUnavailable):
		return "", fmt.Errorf("%w: %w", fleetrepush.ErrNoToken, err)
	case err != nil:
		return "", err
	}
	return tok.AccessToken, nil
}

// repushTokenProvider adapts store.AccountRepo to telemetry.TeslaTokenProvider.
type repushTokenProvider struct {
	repo *store.AccountRepo
}

func (a *repushTokenProvider) GetTeslaToken(ctx context.Context, userID string) (telemetry.TeslaToken, error) {
	tok, err := a.repo.GetTeslaToken(ctx, userID)
	if err != nil {
		return telemetry.TeslaToken{}, err
	}
	out := telemetry.TeslaToken{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken}
	if tok.ExpiresAt != nil {
		out.ExpiresAt = time.Unix(*tok.ExpiresAt, 0)
	}
	return out, nil
}

// repushTokenUpdater persists a refreshed pair. THE ONE WRITE A DRY RUN CAN
// STILL MAKE, for the fleetorphan reason: an OAuth refresh invalidates the
// stored refresh token at Tesla, so not persisting the new pair would break the
// owner's next server-side Tesla call.
type repushTokenUpdater struct {
	repo *store.AccountRepo
}

func (a *repushTokenUpdater) UpdateTeslaToken(
	ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64,
) error {
	return a.repo.UpdateTeslaToken(ctx, userID, accessToken, refreshToken, expiresAt)
}

// repushTokenRotator serializes the refresh through the account row's lock
// (MYR-595). It WAITS for the lock rather than declining: nothing in a batch
// sweep is latency-sensitive, and abandoning would only put it back on the
// unserialized path.
type repushTokenRotator struct {
	repo *store.AccountRepo
}

// repushTokenLockWait bounds the queue for the row lock.
const repushTokenLockWait = 10 * time.Second

func (a *repushTokenRotator) RotateTeslaToken(
	ctx context.Context,
	userID string,
	rotate func(ctx context.Context, stored telemetry.TeslaToken) (telemetry.TeslaToken, bool, error),
) (telemetry.TeslaToken, error) {
	snap, err := a.repo.RotateTeslaTokenLockedWaiting(ctx, userID, repushTokenLockWait,
		func(ctx context.Context, stored store.TeslaTokenSnapshot) (store.TeslaTokenPair, bool, error) {
			next, rotated, rErr := rotate(ctx, repushTokenFromSnapshot(stored))
			if rErr != nil || !rotated {
				return store.TeslaTokenPair{}, false, rErr
			}
			return store.TeslaTokenPair{
				AccessToken:  next.AccessToken,
				RefreshToken: next.RefreshToken,
				ExpiresAt:    next.ExpiresAt.Unix(),
			}, true, nil
		})
	if errors.Is(err, store.ErrTeslaTokenRowBusy) {
		return telemetry.TeslaToken{}, fmt.Errorf("rotate tesla token(user=%s): %w", userID, telemetry.ErrTeslaTokenRowBusy)
	}
	if err != nil {
		return telemetry.TeslaToken{}, err
	}
	return repushTokenFromSnapshot(snap), nil
}

// repushTokenFromSnapshot maps the column's 0/NULL expiry onto the zero time
// that means "unknown" in the telemetry layer.
func repushTokenFromSnapshot(snap store.TeslaTokenSnapshot) telemetry.TeslaToken {
	tok := telemetry.TeslaToken{AccessToken: snap.AccessToken, RefreshToken: snap.RefreshToken}
	if snap.ExpiresAt != 0 {
		tok.ExpiresAt = time.Unix(snap.ExpiresAt, 0)
	}
	return tok
}
