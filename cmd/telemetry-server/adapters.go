package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

// vinResolverAdapter adapts store.VehicleRepo (returns Vehicle) to the
// ws.VINResolver interface (returns vehicleID string).
type vinResolverAdapter struct {
	repo *store.VehicleRepo
}

func (a *vinResolverAdapter) GetByVIN(ctx context.Context, vin string) (string, error) {
	v, err := a.repo.GetByVIN(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve VIN: %w", err)
	}
	return v.ID, nil
}

// vehicleOwnerAdapter adapts store.VehicleRepo to the
// telemetry.VehicleOwnerLookup interface (returns owning user ID).
type vehicleOwnerAdapter struct {
	repo *store.VehicleRepo
}

func (a *vehicleOwnerAdapter) GetVehicleOwner(ctx context.Context, vin string) (string, error) {
	v, err := a.repo.GetByVIN(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve vehicle owner: %w", err)
	}
	return v.UserID, nil
}

// proxyHTTPClient returns an *http.Client suitable for calling the
// tesla-http-proxy sidecar. When the proxy URL is a loopback address
// (127.0.0.1 or localhost), TLS certificate verification is skipped
// because the proxy uses a self-signed cert and traffic never leaves
// the machine. This is the standard pattern for tesla-http-proxy
// sidecar deployments (TeslaMate, Home Assistant, etc.).
//
// For non-loopback URLs, returns nil (FleetAPIClient uses its default client).
func proxyHTTPClient(proxyURL string, logger *slog.Logger) *http.Client {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil
	}

	host := u.Hostname()
	ip := net.ParseIP(host)
	isLoopback := host == "localhost" || (ip != nil && ip.IsLoopback())

	if !isLoopback {
		return nil
	}

	logger.Info("proxy on loopback — skipping TLS verification",
		slog.String("proxy_url", proxyURL),
	)

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // loopback only — see guard above
			},
		},
	}
}
