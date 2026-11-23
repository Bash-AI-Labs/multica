#!/bin/sh
# Multica only calls hooks over HTTPS, and a container on the compose network
# has no public certificate. The first start issues a self-signed one for the
# service's network alias; the backend trusts exactly that file through
# MULTICA_PLUGIN_DEV_CA.
set -eu

host="${PREVIEW_REVIEW_HOST:-preview-review.internal}"
dir="${TLS_DIR:-/tls}"

if [ ! -s "$dir/cert.pem" ] || [ ! -s "$dir/key.pem" ] || [ "$(cat "$dir/host" 2>/dev/null)" != "$host" ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -subj "/CN=$host" -addext "subjectAltName=DNS:$host" \
    -keyout "$dir/key.pem" -out "$dir/cert.pem" 2>/dev/null
  printf '%s' "$host" > "$dir/host"
  echo "issued a TLS certificate for $host"
fi

exec node /app/handler.mjs
