package simulator

import (
	"crypto/tls"
	"net/http"
)

// tlsHTTPClient creates an http.Client with the provided TLS configuration.
// Used by Vehicle.dial to establish mTLS WebSocket connections.
func tlsHTTPClient(cfg *tls.Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: cfg,
		},
	}
}
