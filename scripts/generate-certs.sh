#!/usr/bin/env bash
set -euo pipefail

# Generate TLS certificates for Tesla Fleet Telemetry
# Usage: ./scripts/generate-certs.sh <domain>
# Example: ./scripts/generate-certs.sh myrobotaxi.app

DOMAIN="${1:?Usage: $0 <domain>}"
CERTS_DIR="./certs"

mkdir -p "$CERTS_DIR"

echo "=== Generating EC private key (secp256r1) ==="
openssl ecparam -name prime256v1 -genkey -noout -out "$CERTS_DIR/private-key.pem"

echo "=== Deriving public key ==="
openssl ec -in "$CERTS_DIR/private-key.pem" -pubout -out "$CERTS_DIR/public-key.pem"

echo "=== Generating CSR ==="
openssl req -new \
  -key "$CERTS_DIR/private-key.pem" \
  -out "$CERTS_DIR/server.csr" \
  -subj "/CN=$DOMAIN"

echo "=== Generating self-signed cert (LOCAL DEV ONLY) ==="
openssl x509 -req \
  -in "$CERTS_DIR/server.csr" \
  -signkey "$CERTS_DIR/private-key.pem" \
  -out "$CERTS_DIR/server.crt" \
  -days 365

echo ""
echo "=== Generated files ==="
ls -la "$CERTS_DIR/"

echo ""
echo "=== Next steps ==="
echo "1. Host public key at: https://$DOMAIN/.well-known/appspecific/com.tesla.3p.public-key.pem"
echo "2. Register app at: https://developer.tesla.com"
echo "3. For production TLS, use Let's Encrypt:"
echo "   certbot certonly -d $DOMAIN --csr $CERTS_DIR/server.csr"
echo "4. Configure fleet telemetry via Tesla Fleet API"
echo "5. Vehicle owners pair virtual key at: https://tesla.com/_ak/$DOMAIN"
