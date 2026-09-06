package ws

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

const (
	// sendBufSize is the capacity of the per-client send channel.
	// When the channel is full, the oldest message is dropped.
	sendBufSize = 64

	// readLimit is the maximum size of a client-to-server message.
	// Clients only send auth + keep-alive; 4 KiB is more than enough.
	readLimit = 4096
)

// Client represents a single authenticated WebSocket connection from a
// browser. Each client has its own send channel and read/write pumps.
type Client struct {
	conn       *websocket.Conn
	userID     string
	vehicleIDs []string // vehicles this user is authorized to see
	// allVehicles is the explicit "this client is authorized for every
	// vehicle" flag. It is set ONLY by the handshake when GetUserVehicles
	// returns the WildcardVehicleID sentinel (dev-mode NoopAuthenticator).
	// Production authenticators MUST NOT return that sentinel, so on
	// production this field stays false and an empty vehicleIDs slice
	// means deny-all per NFR-3.21.
	allVehicles bool
	// roles is this client's vehicleID -> role table, published as an
	// IMMUTABLE MAP behind an atomic pointer. Seeded at handshake time
	// alongside vehicleIDs (handler.go authenticateClient) and REPLACED
	// WHOLE — never mutated in place — by the access revalidator when a
	// window edge changes what a caller may see (MYR-602).
	//
	// Per websocket-protocol.md §4.6 the hub looks the role up here to pick
	// the role-appropriate pre-marshaled frame to enqueue. A missing entry
	// resolves to the empty Role("") sentinel and the hub treats it as
	// deny-all (fail-closed).
	//
	// WHY AN ATOMIC SWAP AND NOT A MUTEX. This is read on the broadcast hot
	// path — twice per BroadcastMasked call per client, at up to one frame
	// per second per car — and it was previously a plain map written once at
	// handshake and thereafter read with no synchronisation at all. That was
	// safe only while nothing ever wrote to it again, which is exactly the
	// property MYR-602 had to give up: a trip window opens and closes on the
	// CLOCK, so a role can change under a live connection with no mutation
	// anywhere to hang a handshake off. Replacing the whole map under an
	// atomic pointer keeps the read lock-free (one atomic load) while making
	// the write a publish rather than a data race, and readers see either the
	// whole old table or the whole new one — never a half-updated one, which
	// for a role table would mean a caller masked as two different tiers on
	// two vehicles in the same frame.
	roles atomic.Pointer[roleTable]
	// rolesMu serialises READ-MODIFY-WRITE of the table above. The atomic
	// pointer makes a single publish tear-free; it does nothing at all about
	// two writers that each read the old table, each compute a new one from
	// it, and each publish — the second silently discarding the first.
	//
	// That is not hypothetical here. The AccessRevalidator's SweepOnce is
	// reachable from three places at once (its own 60-second ticker, and the
	// trips service's nudge on every window edge and every participant added),
	// and its re-mask is exactly such a read-modify-write. Two overlapping
	// passes could therefore have a NARROWING publish overwritten by a
	// concurrent pass holding a pre-narrowing resolution — leaving a
	// participant whose window closed with live location until something else
	// happened to re-mask them. Held across the resolve as well as the publish,
	// so the losing pass re-reads the database after the winner's write rather
	// than replaying a decision made before it.
	rolesMu sync.Mutex
	// defaultRole is the fallback role consulted ONLY when allVehicles=true
	// and the per-vehicle roles table has no entry for the requested
	// vehicleID. Set by the handshake to auth.RoleOwner for the dev-mode
	// NoopAuthenticator path (whose ResolveRole returns RoleOwner
	// unconditionally) so dev-mode clients receive role-projected frames
	// for every vehicle the server is broadcasting for, instead of the
	// empty Role("") deny-all sentinel that left them silently filtered
	// out (MYR-66). Production clients have allVehicles=false; defaultRole
	// is never consulted, so the fail-closed deny-all posture for clients
	// without an explicit roles entry is preserved.
	defaultRole auth.Role
	// subscribed tracks which of the client's owned vehicles are
	// currently active subscriptions. Initialized at handshake from
	// vehicleIDs (so a client that never sends subscribe/unsubscribe
	// receives every owned vehicle, matching pre-MYR-46 behavior).
	// subscribe ADDS to the set after an ownership check; unsubscribe
	// REMOVES. Mutations are guarded by subMu, NOT by Hub.mu, so the
	// per-VIN broadcast hot-path (Hub.RLock) does not contend with the
	// readPump.
	subscribed map[string]struct{}
	subMu      sync.RWMutex
	// revoked marks this session as torn down for an access reason
	// (MYR-373). Set the instant a revocation is processed and BEFORE the
	// close frame is written, because the graceful WebSocket close waits for
	// the peer to echo — up to five seconds — and a viewer whose grant was
	// just pulled must not receive another GPS frame during that handshake.
	// Read on the broadcast hot path via hasVehicle; an atomic rather than a
	// mutex so the fan-out stays lock-free per client.
	revoked atomic.Bool
	// done is closed exactly once, by markRevoked, to wake writePump when a
	// session is torn down for an access reason.
	//
	// It exists because the revoked flag alone DEADLOCKS the teardown. Once
	// enqueue refuses everything, nothing can ever arrive on c.send again,
	// and writePump blocks on exactly three things: ctx.Done, c.send closing,
	// and a failed write. None of them can happen. gctx is cancelled only
	// after g.Wait() returns (handler.go), g.Wait() waits for writePump, and
	// Unregister — the only closer of c.send — runs after that. Circular
	// wait: the session's goroutines and its hub entry survive forever.
	//
	// Before the enqueue guard, the 15s heartbeat was the accidental escape
	// hatch: BroadcastAll reached c.send, writePump woke, the write failed on
	// the dead connection, and teardown followed. The guard closed that door,
	// so the wake-up has to be explicit.
	done     chan struct{}
	doneOnce sync.Once

	remoteAddr string
	send       chan []byte
	hub        *Hub
	logger     *slog.Logger
}

// newClient creates a Client that is not yet authenticated. The userID and
// vehicleIDs are populated after the auth handshake completes.
func newClient(conn *websocket.Conn, hub *Hub, logger *slog.Logger) *Client {
	c := &Client{
		conn:       conn,
		send:       make(chan []byte, sendBufSize),
		done:       make(chan struct{}),
		subscribed: make(map[string]struct{}),
		hub:        hub,
		logger:     logger,
	}
	c.setRoles(nil)
	return c
}

// markRevoked cuts this session off and wakes its writePump, in that order.
//
// The two halves are inseparable and that is why this is one method rather
// than two statements at the call site: setting the flag without signalling
// strands writePump forever (see the `done` field), and signalling without
// setting the flag would let a frame slip out between the two.
//
// Idempotent — a second revocation, or the backstop sweep landing on top of a
// nudge, must not panic on a double close. `done` is separate from `send`, so
// this never races Unregister's or Stop's close of the send channel.
func (c *Client) markRevoked() {
	c.revoked.Store(true)
	c.doneOnce.Do(func() { close(c.done) })
}
