#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ "$#" -ne 1 ]]; then
  echo "Usage: deploy/bin/bootstrap-runner-trust.sh IDENTITY_DIRECTORY" >&2
  exit 2
fi

identity_directory="$1"
: "${SECONDBOX_RUNNER_ID:?set SECONDBOX_RUNNER_ID}"
: "${SECONDBOX_RUNNER_CA_CERTIFICATE:?set SECONDBOX_RUNNER_CA_CERTIFICATE}"

if [[ "$identity_directory" != /* || -L "$identity_directory" ]]; then
  echo "Runner identity directory must be an absolute non-symbolic-link path" >&2
  exit 1
fi
if [[ -L "$SECONDBOX_RUNNER_CA_CERTIFICATE" || ! -f "$SECONDBOX_RUNNER_CA_CERTIFICATE" ]]; then
  echo "SECONDBOX_RUNNER_CA_CERTIFICATE must be a regular non-symbolic-link file" >&2
  exit 1
fi

validate_identity() {
  local directory="$1"
  for name in runner.crt runner.key runner-ca.crt; do
    if [[ -L "$directory/$name" || ! -f "$directory/$name" ]]; then
      echo "Runner identity is incomplete: $directory/$name" >&2
      return 1
    fi
  done
  if ! cmp -s "$SECONDBOX_RUNNER_CA_CERTIFICATE" "$directory/runner-ca.crt"; then
    echo "Runner identity CA differs from the configured control-plane CA" >&2
    return 1
  fi
  openssl verify -CAfile "$directory/runner-ca.crt" "$directory/runner.crt" >/dev/null
  local certificate_public_key
  local private_public_key
  certificate_public_key="$(openssl x509 -in "$directory/runner.crt" -pubkey -noout)"
  private_public_key="$(openssl pkey -in "$directory/runner.key" -pubout)"
  if [[ "$certificate_public_key" != "$private_public_key" ]]; then
    echo "Runner certificate and private key do not match" >&2
    return 1
  fi
  if ! openssl x509 -in "$directory/runner.crt" -noout -ext subjectAltName |
      grep -Fq "URI:spiffe://secondbox/runner/$SECONDBOX_RUNNER_ID"; then
    echo "Runner certificate identity does not match SECONDBOX_RUNNER_ID" >&2
    return 1
  fi
}

if [[ -e "$identity_directory" ]]; then
  if [[ ! -d "$identity_directory" ]]; then
    echo "Runner identity target exists and is not a directory" >&2
    exit 1
  fi
  validate_identity "$identity_directory"
  echo "Runner trust already bootstrapped: $identity_directory"
  exit 0
fi

: "${SECONDBOX_RUNNER_CA_PRIVATE_KEY:?set SECONDBOX_RUNNER_CA_PRIVATE_KEY}"
: "${SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS:?set SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS}"
if [[ -L "$SECONDBOX_RUNNER_CA_PRIVATE_KEY" || ! -f "$SECONDBOX_RUNNER_CA_PRIVATE_KEY" ]]; then
  echo "SECONDBOX_RUNNER_CA_PRIVATE_KEY must be a regular non-symbolic-link file" >&2
  exit 1
fi
if [[ ! "$SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS" =~ ^[0-9]+$ ]] ||
   (( SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS < 1 )); then
  echo "SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS must be a positive integer" >&2
  exit 1
fi
ca_certificate_public_key="$(openssl x509 -in "$SECONDBOX_RUNNER_CA_CERTIFICATE" -pubkey -noout)"
ca_private_public_key="$(openssl pkey -in "$SECONDBOX_RUNNER_CA_PRIVATE_KEY" -pubout)"
if [[ "$ca_certificate_public_key" != "$ca_private_public_key" ]]; then
  echo "Runner CA certificate and private key do not match" >&2
  exit 1
fi
for required_command in openssl install mktemp mv chmod cmp grep rm; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "Runner trust bootstrap requires command: $required_command" >&2
    exit 1
  fi
done

identity_parent="$(dirname "$identity_directory")"
install -d -m 700 "$identity_parent"
staging_directory="$(mktemp -d "$identity_parent/.runner-identity.XXXXXX")"
cleanup() {
  local status="$?"
  if ! rm -rf -- "$staging_directory"; then
    echo "Failed to remove runner identity staging directory: $staging_directory" >&2
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT

openssl genpkey \
  -algorithm RSA \
  -pkeyopt rsa_keygen_bits:3072 \
  -out "$staging_directory/runner.key" 2>/dev/null
openssl req \
  -new \
  -sha256 \
  -key "$staging_directory/runner.key" \
  -subj "/CN=$SECONDBOX_RUNNER_ID" \
  -out "$staging_directory/runner.csr"
printf '%s\n' \
  'basicConstraints=critical,CA:FALSE' \
  'keyUsage=critical,digitalSignature' \
  'extendedKeyUsage=clientAuth' \
  "subjectAltName=URI:spiffe://secondbox/runner/$SECONDBOX_RUNNER_ID" \
  >"$staging_directory/runner.ext"
certificate_serial="0x$(openssl rand -hex 16)"
openssl x509 \
  -req \
  -sha256 \
  -days "$SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS" \
  -set_serial "$certificate_serial" \
  -in "$staging_directory/runner.csr" \
  -CA "$SECONDBOX_RUNNER_CA_CERTIFICATE" \
  -CAkey "$SECONDBOX_RUNNER_CA_PRIVATE_KEY" \
  -extfile "$staging_directory/runner.ext" \
  -out "$staging_directory/runner.crt" 2>/dev/null
install -m 644 "$SECONDBOX_RUNNER_CA_CERTIFICATE" "$staging_directory/runner-ca.crt"
chmod 600 "$staging_directory/runner.key" "$staging_directory/runner.crt"
rm "$staging_directory/runner.csr" "$staging_directory/runner.ext"
validate_identity "$staging_directory"
mv "$staging_directory" "$identity_directory"
trap - EXIT
echo "Bootstrapped create-only runner trust: $identity_directory"
