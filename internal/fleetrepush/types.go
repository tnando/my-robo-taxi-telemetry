// Collaborators, configuration and the report shape for the MYR-630 sweep.
// The narrative lives in doc.go.

package fleetrepush

import (
	"context"
	"errors"
	"time"
)

// Candidate is one car the sweep may act on. It mirrors
// store.StreamingFleetVehicle, re-typed at the boundary so this package holds
// no internal/store import (the fleetorphan precedent).
type Candidate struct {
	VehicleID       string
	VIN             string
	UserID          string
	VehicleName     string
	LastUpdated     time.Time
	Status          string
	Suspended       bool
	ConfigAbsent    bool
	PendingOwnerAck bool
}

// ConfigStatus is Tesla's answer to "is there a config on this car, and when
// does it expire?".
type ConfigStatus struct {
	// Configured is true when Tesla reports EITHER synced or a non-nil config
	// body — a pushed-but-unapplied config is still a config on file, and the
	// fleetorphan sweep reads the same pair for the same reason.
	Configured bool
	// Exp is Tesla's echoed expiry, best-effort: nil means the age is unknown.
	Exp *int64
}

// Store lists the sweep's candidates.
type Store interface {
	ListStreamingFleetConfigVehicles(ctx context.Context, limit int) ([]Candidate, error)
}

// TokenSource resolves an owner's Tesla access token. Implementations MUST go
// through the serialized rotation (MYR-595), never an unguarded refresh: this
// sweep walks many owners' tokens from a batch job while the live server
// refreshes the same accounts on demand, and a refresh token spent twice hands
// a user a spurious "re-link your Tesla account".
type TokenSource interface {
	AccessToken(ctx context.Context, userID string) (string, error)
}

// ConfigReader reads a car's current config. Unsigned — the direct Fleet API
// base URL, never the signing proxy.
type ConfigReader interface {
	GetTelemetryConfig(ctx context.Context, token, vin string) (ConfigStatus, error)
}

// Pusher sends the current DefaultFieldConfig to one VIN. Signed — the
// tesla-http-proxy. Implementations must classify Tesla's 200-with-skips as an
// error (telemetry.SkipErrorFor), or an unpaired car reads as a success.
type Pusher interface {
	PushForVIN(ctx context.Context, token, vin string) error
}

// SkipClassifier answers whether a push error is Tesla declining because the
// virtual key is not enrolled. Separate from Pusher so this package need not
// import internal/telemetry for one errors.As.
type SkipClassifier interface {
	IsAwaitingVirtualKey(err error) bool
}

// Auditor records the MYR-447 operator-decrypt row for one owner's Tesla
// credentials. Called once per owner, BEFORE the token is read.
type Auditor interface {
	RecordTokenDecrypt(ctx context.Context, userID string) error
}

// Deps are the sweep's collaborators.
type Deps struct {
	Store    Store
	Tokens   TokenSource
	Reader   ConfigReader
	Pusher   Pusher
	Classify SkipClassifier
	Auditor  Auditor
}

// ErrNoToken means no Tesla token is on file for the owner — a permanently
// unpushable car, not a transient failure. TokenSource implementations wrap it.
var ErrNoToken = errors.New("no tesla token on file")

// ErrNoConfig means Tesla reports nothing configured for the VIN (including a
// 404). ConfigReader implementations return it.
var ErrNoConfig = errors.New("no fleet-telemetry config")

// Config is the sweep's operator-facing configuration.
type Config struct {
	// Apply performs the pushes. False — the default — is a dry run.
	Apply bool
	// Limit caps how many vehicles ONE RUN examines, which is also the ceiling
	// on pushes. Skips count against it: a run that spends its budget on
	// suspended cars is a run whose report says so, which is preferable to a
	// cap whose meaning depends on the fleet's health.
	Limit int
	// Interval is the minimum gap between Tesla round-trips. Zero uses
	// DefaultInterval; a test passes a negative to disable the wait.
	Interval time.Duration
	// Now is injectable for tests. Nil uses time.Now.
	Now func() time.Time
}

// Defaults for a run that names nothing.
const (
	// DefaultLimit bounds one run. The whole fleet is well under this today;
	// the cap exists so a runaway can only ever be one run long.
	DefaultLimit = 50
	// DefaultInterval is the one-per-second rate limit. Tesla's per-app rate
	// limits are not published per endpoint, so the sweep paces itself at a
	// rate no reasonable budget objects to rather than discovering the ceiling.
	DefaultInterval = time.Second
	// configLifetime is the `exp` every push in this codebase sets: 350 days.
	// It is what dates a config — see doc.go.
	configLifetime = 350 * 24 * time.Hour
)

// Actions recorded per vehicle.
const (
	ActionWouldPush = "would_push"
	ActionPushed    = "pushed"
	ActionSkipped   = "skipped"
	ActionFailed    = "failed"
)

// Reasons. Skips are states the sweep declines to act on; failures are things
// that went wrong and may not next time.
const (
	ReasonOwnerSuspended   = "owner_suspended"
	ReasonConfigAbsent     = "config_absent"
	ReasonAwaitingOwnerAck = "awaiting_owner_ack"
	ReasonNoToken          = "no_token"
	ReasonNoConfig         = "no_config"
	ReasonMissingKey       = "missing_key"
	ReasonTokenFailed      = "token_failed"
	ReasonConfigReadFailed = "config_read_failed"
	ReasonPushFailed       = "push_failed"
)

// VehicleReport is one line of the report.
type VehicleReport struct {
	VIN         string    `json:"vin"`
	UserID      string    `json:"userId"`
	VehicleName string    `json:"vehicleName,omitempty"`
	Action      string    `json:"action"`
	Reason      string    `json:"reason,omitempty"`
	LastUpdated time.Time `json:"lastUpdated"`
	// ConfigAgeDays is how long ago the current config was pushed, derived from
	// Tesla's echoed exp. Nil when Tesla did not echo one, or when the sweep
	// never got as far as reading it.
	ConfigAgeDays *float64 `json:"configAgeDays,omitempty"`
	// Error is the redacted failure text on a failed line.
	Error string `json:"error,omitempty"`
}

// Report is the whole run, printed as JSON.
type Report struct {
	Mode     string `json:"mode"`
	Limit    int    `json:"limit"`
	Examined int    `json:"examined"`
	// Pushed counts real pushes; WouldPush counts the dry run's intentions.
	// Two fields rather than one, so a report can never be misread as having
	// changed something it did not.
	Pushed         int             `json:"pushed"`
	WouldPush      int             `json:"wouldPush"`
	Skipped        int             `json:"skipped"`
	Failed         int             `json:"failed"`
	SkipReasons    map[string]int  `json:"skipReasons,omitempty"`
	FailureReasons map[string]int  `json:"failureReasons,omitempty"`
	Vehicles       []VehicleReport `json:"vehicles"`
}
