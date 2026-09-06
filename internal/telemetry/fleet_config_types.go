package telemetry

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// tokenValidator validates JWT tokens and returns the authenticated user ID.
// Matches auth.JWTAuthenticator.ValidateToken.
type tokenValidator interface {
	ValidateToken(ctx context.Context, token string) (userID string, err error)
}

// VehicleOwnerLookup resolves a VIN to its owning user ID. Implementations
// should return an error wrapping sdk.ErrNotFound when the VIN is not
// registered.
type VehicleOwnerLookup interface {
	GetVehicleOwner(ctx context.Context, vin string) (userID string, err error)
}

// TeslaTokenProvider retrieves the Tesla OAuth access token for a user.
// The token is read from the database (stored during Tesla account linking).
type TeslaTokenProvider interface {
	GetTeslaToken(ctx context.Context, userID string) (TeslaToken, error)
}

// TeslaTokenRefresher refreshes an expired Tesla OAuth token using a
// stored refresh_token. Implementations should call Tesla's OAuth2 endpoint.
type TeslaTokenRefresher interface {
	Refresh(ctx context.Context, refreshToken string) (TeslaRefreshedToken, error)
}

// TeslaTokenUpdater persists a refreshed Tesla token set to the database.
type TeslaTokenUpdater interface {
	UpdateTeslaToken(ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64) error
}

// TeslaToken holds a Tesla OAuth2 access token with its expiry.
type TeslaToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time // zero value means no expiry info
}

// EndpointConfig describes the telemetry server that vehicles
// should connect to after fleet config is pushed.
type EndpointConfig struct {
	Hostname string
	Port     int
	CA       string // PEM-encoded CA cert
}

// FleetConfigOption configures optional dependencies on FleetConfigHandler.
type FleetConfigOption func(*FleetConfigHandler)

// WithTokenRefresher enables automatic Tesla token refresh when a token is
// expired. Both refresher and updater must be provided for auto-refresh to
// work. The updater persists the refreshed token to the database.
func WithTokenRefresher(refresher TeslaTokenRefresher, updater TeslaTokenUpdater) FleetConfigOption {
	return func(h *FleetConfigHandler) {
		h.refresher = refresher
		h.updater = updater
	}
}

// DriverAccessGate answers, for one VIN, whether the car is driver-linked with
// its owner-approval acknowledgment still outstanding (MYR-599). Satisfied by
// *store.VehicleRepo.PendingDriverAcknowledgmentByVIN.
//
// Consumer-site interface, and the error is deliberately part of it: this gate
// protects somebody who is not our user, so a caller that cannot tell must
// refuse rather than proceed. Folding the failure into the bool would make
// fail-open the path of least resistance.
type DriverAccessGate interface {
	PendingDriverAcknowledgmentByVIN(ctx context.Context, vin string) (pending bool, err error)
}

// WithDriverAccessGate installs the MYR-599 consent gate on the VIN-keyed push
// route.
//
// WHY IT IS AN OPTION AND NOT A CONSTRUCTOR PARAMETER, stated plainly because
// "optional consent gate" deserves the scrutiny: the vehicleId-keyed route —
// the one a client can actually reach, since browsers and the app receive VINs
// masked to their last 4 — gates UNCONDITIONALLY on the driver-access row it
// already holds from GetByID, with no option and no way to omit it. This option
// covers the VIN-keyed sibling, whose caller must already possess a full VIN and
// is in practice the bench and ops surface. cmd/ wires it always; leaving it out
// is a dev/test configuration, and pushForVIN says so in its log.
func WithDriverAccessGate(gate DriverAccessGate) FleetConfigOption {
	return func(h *FleetConfigHandler) { h.driverAccess = gate }
}

// WithTokenRotator serializes the refresh through the account row's lock
// (MYR-595), so two pushes racing for one owner cannot both spend the same
// single-use refresh token and hand one of them a spurious "re-link your Tesla
// account". Without it the refresh runs the old unserialized way.
func WithTokenRotator(rotator TeslaTokenRotator) FleetConfigOption {
	return func(h *FleetConfigHandler) { h.rotator = rotator }
}

// extractBearerToken extracts the token from an "Authorization: Bearer <token>"
// header. Returns empty string if the header is missing or malformed.
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return auth[len(prefix):]
}

// fleetConfigResponse is the JSON body returned on a successful config push.
type fleetConfigResponse struct {
	Status string `json:"status"`
	VIN    string `json:"vin"`
}
