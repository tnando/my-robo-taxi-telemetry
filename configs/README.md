# Configuration

Place configuration files here. Template:

```json
{
  "server": {
    "tesla_port": 443,
    "client_port": 8080,
    "metrics_port": 9090
  },
  "tls": {
    "cert_file": "/certs/server.crt",
    "key_file": "/certs/server.key",
    "ca_file": "/certs/tesla-ca.pem"
  },
  "database": {
    "max_conns": 20,
    "min_conns": 5
  },
  "telemetry": {
    "max_vehicles": 100,
    "event_buffer_size": 1000,
    "batch_write_interval": "5s",
    "batch_write_size": 100
  },
  "drives": {
    "min_duration": "2m",
    "min_distance_miles": 0.1,
    "end_debounce": "30s",
    "geocode_timeout": "5s"
  },
  "websocket": {
    "heartbeat_interval": "15s",
    "write_timeout": "10s",
    "max_connections_per_user": 5,
    "read_limit": 4096
  },
  "auth": {
    "token_issuer": "myrobotaxi",
    "token_audience": "telemetry"
  }
}
```

## Environment Variables (Required)

| Variable | Description |
|----------|-------------|
| `DATABASE_URL` | PostgreSQL connection string |
| `AUTH_SECRET` | Shared secret with NextAuth.js for JWT validation |
| `MAPBOX_TOKEN` | Mapbox API token for reverse geocoding |
| `TLS_CERT_FILE` | Path to TLS certificate |
| `TLS_KEY_FILE` | Path to TLS private key |
| `TLS_CA_FILE` | Path to Tesla CA certificate |
