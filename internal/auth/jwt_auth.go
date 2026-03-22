// Package auth provides JWT-based authentication for WebSocket clients.
// It implements the ws.Authenticator interface using HS256 JWTs signed by
// the same AUTH_SECRET the Next.js frontend uses.
package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors returned by JWTAuthenticator.
var (
	// ErrInvalidToken indicates the JWT is malformed, expired, or signed
	// with the wrong key.
	ErrInvalidToken = errors.New("invalid token")

	// ErrMissingSubject indicates the JWT has no "sub" claim.
	ErrMissingSubject = errors.New("missing subject claim")
)

// queryUserVehicleIDs fetches all vehicle IDs belonging to a user from
// the Prisma-owned "Vehicle" table.
const queryUserVehicleIDs = `SELECT "id" FROM "Vehicle" WHERE "userId" = $1`

// JWTAuthenticator validates HS256 JWTs and resolves the authenticated
// user's vehicle IDs from the database. It caches vehicle lookups to
// avoid hitting the DB on every WebSocket reconnect.
type JWTAuthenticator struct {
	secret []byte
	cache  *vehicleCache
}

// NewJWTAuthenticator creates an authenticator that verifies HS256 JWTs
// using the given secret and queries the pool for vehicle ownership.
func NewJWTAuthenticator(secret string, pool *pgxpool.Pool) *JWTAuthenticator {
	querier := &pgVehicleQuerier{pool: pool}
	return &JWTAuthenticator{
		secret: []byte(secret),
		cache:  newVehicleCache(querier, vehicleCacheTTL),
	}
}

// ValidateToken parses and verifies an HS256 JWT, checks expiration, and
// returns the user ID from the "sub" claim.
func (a *JWTAuthenticator) ValidateToken(_ context.Context, token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("auth.ValidateToken: %w", ErrInvalidToken)
	}

	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return a.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))

	if err != nil {
		return "", fmt.Errorf("auth.ValidateToken: %w: %w", ErrInvalidToken, err)
	}

	sub, err := parsed.Claims.GetSubject()
	if err != nil || sub == "" {
		return "", fmt.Errorf("auth.ValidateToken: %w", ErrMissingSubject)
	}

	return sub, nil
}

// GetUserVehicles returns the vehicle IDs (Prisma cuids) that the user is
// authorized to receive telemetry for. Results are cached for 5 minutes.
func (a *JWTAuthenticator) GetUserVehicles(ctx context.Context, userID string) ([]string, error) {
	ids, err := a.cache.lookup(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("auth.GetUserVehicles(user=%s): %w", userID, err)
	}
	return ids, nil
}

// pgVehicleQuerier queries PostgreSQL for a user's vehicle IDs.
type pgVehicleQuerier struct {
	pool *pgxpool.Pool
}

// GetUserVehicleIDs queries the "Vehicle" table for all vehicles belonging
// to the given user.
func (q *pgVehicleQuerier) GetUserVehicleIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := q.pool.Query(ctx, queryUserVehicleIDs, userID)
	if err != nil {
		return nil, fmt.Errorf("pgVehicleQuerier.GetUserVehicleIDs: query: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("pgVehicleQuerier.GetUserVehicleIDs: scan: %w", err)
		}
		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgVehicleQuerier.GetUserVehicleIDs: rows: %w", err)
	}

	return ids, nil
}
