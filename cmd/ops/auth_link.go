package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

const (
	// defaultCallbackPort is the local HTTP port the CLI listens on for the
	// Tesla OAuth redirect. This port must be registered as an allowed
	// redirect URI on the Tesla Fleet API app: http://localhost:<port>/callback.
	defaultCallbackPort = 8765

	// defaultOAuthTimeout bounds how long the CLI waits for the user to
	// complete the browser flow before giving up.
	defaultOAuthTimeout = 2 * time.Minute

	// teslaOAuthAuthorizeURL is Tesla's OAuth2 authorize endpoint.
	teslaOAuthAuthorizeURL = "https://auth.tesla.com/oauth2/v3/authorize"

	// teslaOAuthTokenURL is Tesla's OAuth2 token endpoint (same one used
	// by refresh_token grant).
	teslaOAuthTokenURL = "https://auth.tesla.com/oauth2/v3/token" //#nosec G101 -- public OAuth endpoint URL, not a credential

	// defaultTeslaOAuthScopes is the Fleet API scope set needed by the
	// ops CLI (read telemetry + push fleet config commands).
	defaultTeslaOAuthScopes = "openid offline_access vehicle_device_data vehicle_cmds vehicle_charging_cmds"
)

// authLinkOutput is the JSON shape printed on success.
type authLinkOutput struct {
	UserID    string `json:"userId"`
	ExpiresAt string `json:"expiresAt"`
	Message   string `json:"message"`
}

// runAuthLink drives the full Tesla OAuth2 authorization_code + PKCE flow:
// spins up a localhost callback server, opens the Tesla authorize URL in
// the developer's browser, exchanges the returned code for fresh tokens,
// and persists them to the Account row.
func runAuthLink(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("auth link", flag.ContinueOnError)
	userID := fs.String("user-id", "", "MyRoboTaxi user id (Prisma cuid)")
	port := fs.Int("port", defaultCallbackPort, "local HTTP port for the OAuth callback (must match the redirect URI registered on the Tesla Fleet API app)")
	scopes := fs.String("scopes", defaultTeslaOAuthScopes, "space-separated OAuth scopes to request")
	timeout := fs.Duration("timeout", defaultOAuthTimeout, "how long to wait for the browser flow to complete")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireFlag("user-id", *userID); err != nil {
		return err
	}

	clientID := os.Getenv("AUTH_TESLA_ID")
	clientSecret := os.Getenv("AUTH_TESLA_SECRET")
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("AUTH_TESLA_ID and AUTH_TESLA_SECRET must be set to link a Tesla account")
	}

	logger := newLogger()
	db, err := openDB(ctx, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	pkce, err := newPKCE()
	if err != nil {
		return fmt.Errorf("generate pkce: %w", err)
	}
	state, err := randomURLSafeString(24)
	if err != nil {
		return fmt.Errorf("generate state: %w", err)
	}

	redirectURI := fmt.Sprintf("http://localhost:%d/callback", *port)
	authorizeURL := buildAuthorizeURL(clientID, redirectURI, *scopes, state, pkce.challenge)

	code, err := runCallbackServer(ctx, logger, *port, state, authorizeURL, *timeout)
	if err != nil {
		return err
	}

	tok, err := exchangeCodeForToken(ctx, logger, clientID, clientSecret, redirectURI, code, pkce.verifier)
	if err != nil {
		return fmt.Errorf("exchange code for token: %w", err)
	}

	accountRepo := store.NewAccountRepo(db.Pool())
	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if err := accountRepo.UpdateTeslaToken(ctx, *userID, tok.AccessToken, tok.RefreshToken, expiresAt.Unix()); err != nil {
		return fmt.Errorf("persist tesla token: %w", err)
	}

	logger.Info("tesla account linked",
		slog.String("user_id", *userID),
		slog.Time("expires_at", expiresAt),
	)

	return writeJSON(os.Stdout, authLinkOutput{
		UserID:    *userID,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		Message:   "Tesla account linked — run `ops auth token` to verify",
	})
}

// pkcePair holds a PKCE verifier/challenge pair for a single OAuth flow.
type pkcePair struct {
	verifier  string
	challenge string
}

// newPKCE generates a fresh PKCE verifier + S256 challenge per RFC 7636.
// The verifier is a URL-safe random string; the challenge is
// base64url(sha256(verifier)) without padding.
func newPKCE() (pkcePair, error) {
	verifier, err := randomURLSafeString(32)
	if err != nil {
		return pkcePair{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return pkcePair{verifier: verifier, challenge: challenge}, nil
}

// randomURLSafeString returns n bytes of cryptographic randomness encoded
// with unpadded base64url, producing a string safe to use in URLs and
// query parameters.
func randomURLSafeString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// buildAuthorizeURL constructs the Tesla /oauth2/v3/authorize URL that
// starts the authorization_code + PKCE flow.
func buildAuthorizeURL(clientID, redirectURI, scopes, state, codeChallenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scopes)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	return teslaOAuthAuthorizeURL + "?" + q.Encode()
}

// runCallbackServer starts a one-shot HTTP server on 127.0.0.1:<port>,
// opens the authorize URL in the user's browser, and blocks until either
// the callback fires with a matching state+code, the context is cancelled,
// or the timeout elapses. Returns the authorization code on success.
func runCallbackServer(
	ctx context.Context,
	logger *slog.Logger,
	port int,
	expectedState, authorizeURL string,
	timeout time.Duration,
) (string, error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", fmt.Errorf("listen on port %d (is it registered as a redirect URI on Tesla?): %w", port, err)
	}

	result := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", callbackHandler(expectedState, result))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "ops: waiting on /callback", http.StatusNotFound)
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("callback server error", slog.Any("error", err))
		}
	}()

	fmt.Fprintf(os.Stderr, "\nOpening Tesla login in your browser…\nIf nothing opens, visit this URL manually:\n\n  %s\n\nWaiting for callback on %s …\n\n", authorizeURL, listener.Addr())
	openBrowser(ctx, logger, authorizeURL)

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	select {
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("timed out after %s waiting for Tesla OAuth callback", timeout)
		}
		return "", fmt.Errorf("oauth flow cancelled: %w", waitCtx.Err())
	case r := <-result:
		if r.err != nil {
			return "", r.err
		}
		return r.code, nil
	}
}

// callbackResult carries the outcome of a single callback request back to
// the waiting runCallbackServer goroutine.
type callbackResult struct {
	code string
	err  error
}

// callbackHandler returns an http.HandlerFunc that validates the state
// parameter, surfaces Tesla-reported errors, and forwards the auth code
// through the result channel. The page the user sees reflects the outcome.
func callbackHandler(expectedState string, result chan<- callbackResult) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		if errCode := q.Get("error"); errCode != "" {
			msg := fmt.Sprintf("Tesla rejected the authorization: %s — %s", errCode, q.Get("error_description"))
			respondCallback(w, http.StatusBadRequest, "OAuth failed", msg)
			result <- callbackResult{err: fmt.Errorf("tesla oauth error: %s: %s", errCode, q.Get("error_description"))}
			return
		}

		if gotState := q.Get("state"); gotState != expectedState {
			respondCallback(w, http.StatusBadRequest, "State mismatch", "The OAuth state parameter did not match. Possible CSRF — retry the command.")
			result <- callbackResult{err: errors.New("oauth state mismatch")}
			return
		}

		code := q.Get("code")
		if code == "" {
			respondCallback(w, http.StatusBadRequest, "Missing code", "Tesla did not return an authorization code.")
			result <- callbackResult{err: errors.New("tesla returned no authorization code")}
			return
		}

		respondCallback(w, http.StatusOK, "Tesla account linked", "You can close this tab and return to the terminal.")
		result <- callbackResult{code: code}
	}
}

// respondCallback writes a tiny HTML page confirming the outcome to the
// user's browser. title/body are HTML-escaped before interpolation.
func respondCallback(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	safeTitle := html.EscapeString(title)
	safeBody := html.EscapeString(body)
	fmt.Fprintf(w,
		`<!doctype html><html><head><title>%[1]s</title><meta charset="utf-8"><style>body{font:16px/1.4 system-ui,sans-serif;max-width:480px;margin:48px auto;padding:0 16px;color:#111}h1{font-size:20px}</style></head><body><h1>%[1]s</h1><p>%[2]s</p></body></html>`,
		safeTitle, safeBody,
	)
}

// openBrowser tries to open browserURL in the user's default browser using
// the platform-appropriate command. On failure it logs a warning; the
// user is already told to open the URL manually in the stderr banner.
func openBrowser(ctx context.Context, logger *slog.Logger, browserURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", browserURL)
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", browserURL)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", browserURL)
	default:
		logger.Warn("unsupported platform for auto-open — open the URL manually",
			slog.String("platform", runtime.GOOS),
		)
		return
	}
	if err := cmd.Start(); err != nil {
		logger.Warn("failed to auto-open browser", slog.Any("error", err))
	}
}

// tokenResponse mirrors Tesla's /oauth2/v3/token response body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// exchangeCodeForToken swaps the one-time authorization code (plus PKCE
// verifier) for an access_token / refresh_token pair.
func exchangeCodeForToken(
	ctx context.Context,
	logger *slog.Logger,
	clientID, clientSecret, redirectURI, code, codeVerifier string,
) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", codeVerifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, teslaOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post to token endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		logger.Warn("tesla token exchange failed",
			slog.Int("status", resp.StatusCode),
			slog.String("body", string(body)),
		)
		return nil, fmt.Errorf("tesla returned %d: %s", resp.StatusCode, string(body))
	}

	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return nil, errors.New("tesla response missing access_token or refresh_token")
	}
	return &tok, nil
}
