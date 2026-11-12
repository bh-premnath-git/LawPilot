#!/usr/bin/env bash
set -euo pipefail
# Development-only, short-lived certs. Never commit or use this CA in production.
out="${1:-work/tls}"
umask 077
mkdir -p "$out"
if [[ -e "$out/ca.key" ]]; then
  echo "TLS files already exist at $out; remove this dev directory explicitly to rotate." >&2
  exit 1
fi
openssl req -x509 -newkey rsa:3072 -nodes -days 7 -subj /CN=LawPilot-Development-CA -keyout "$out/ca.key" -out "$out/ca.pem" 2>/dev/null
for identity in api-gateway execution-worker root-supervisor ai-capabilities calendar-service; do
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$identity" -keyout "$out/$identity.key" -out "$out/$identity.csr" 2>/dev/null
  cat > "$out/$identity.ext" <<EXT
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=DNS:$identity,DNS:localhost,IP:127.0.0.1,URI:spiffe://lawpilot.local/$identity
EXT
  openssl x509 -req -days 7 -in "$out/$identity.csr" -CA "$out/ca.pem" -CAkey "$out/ca.key" -CAcreateserial -extfile "$out/$identity.ext" -out "$out/$identity.pem" 2>/dev/null
  rm "$out/$identity.csr" "$out/$identity.ext"
done
chmod 644 "$out/ca.pem" "$out/"*.pem
printf 'Created development mTLS identities in %s (expire in 7 days).\n' "$out"
