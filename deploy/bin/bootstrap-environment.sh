#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ "$#" -ne 1 ]]; then
  echo "Usage: deploy/bin/bootstrap-environment.sh PATH" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
environment_path="$1"
template_path="$repo_root/deploy/environment.example"

if [[ -L "$environment_path" ]]; then
  echo "Refusing symbolic-link environment target: $environment_path" >&2
  exit 1
fi
if [[ -e "$environment_path" && ! -f "$environment_path" ]]; then
  echo "Refusing non-regular environment target: $environment_path" >&2
  exit 1
fi
if [[ ! -e "$environment_path" ]]; then
  install -m 600 "$template_path" "$environment_path"
fi

if ! grep -Eq 'GENERATE_WITH_DEPLOY_BOOTSTRAP|GENERATE_LOCAL_DATABASE_URL|GENERATE_RUNNER_PKI' "$environment_path"; then
  chmod 600 "$environment_path"
  echo "Environment already bootstrapped: $environment_path"
  exit 0
fi

for required_command in openssl awk grep install mktemp mv chmod; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "Bootstrap requires command: $required_command" >&2
    exit 1
  fi
done

runner_server_name="$(
  awk -F= '
    $1 == "SECONDBOX_RUNNER_SERVER_NAME" {
      sub(/^[^=]*=/, "")
      print
      found++
    }
    END {
      if (found != 1) {
        exit 1
      }
    }
  ' "$environment_path"
)" || {
  echo "Bootstrap requires exactly one SECONDBOX_RUNNER_SERVER_NAME" >&2
  exit 1
}

runner_server_san=""
if [[ "$runner_server_name" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  IFS='.' read -r -a runner_server_octets <<<"$runner_server_name"
  for octet in "${runner_server_octets[@]}"; do
    if (( 10#$octet > 255 )); then
      echo "SECONDBOX_RUNNER_SERVER_NAME must be a valid DNS name or IPv4 address" >&2
      exit 1
    fi
  done
  runner_server_san="IP:$runner_server_name"
elif (( ${#runner_server_name} <= 253 )) &&
     [[ "$runner_server_name" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]$ ||
        "$runner_server_name" =~ ^[A-Za-z0-9]$ ]] &&
     [[ "$runner_server_name" != *..* ]]; then
  IFS='.' read -r -a runner_server_labels <<<"$runner_server_name"
  for label in "${runner_server_labels[@]}"; do
    if (( ${#label} > 63 )) ||
       [[ ! "$label" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$ &&
          ! "$label" =~ ^[A-Za-z0-9]$ ]]; then
      echo "SECONDBOX_RUNNER_SERVER_NAME must be a valid DNS name or IPv4 address" >&2
      exit 1
    fi
  done
  runner_server_san="DNS:$runner_server_name"
else
  echo "SECONDBOX_RUNNER_SERVER_NAME must be a valid DNS name or IPv4 address" >&2
  exit 1
fi
if [[ "$runner_server_name" == *GENERATE* || "$runner_server_name" == *REPLACE* ]]; then
  echo "SECONDBOX_RUNNER_SERVER_NAME must not contain a placeholder" >&2
  exit 1
fi

postgres_password="$(openssl rand -hex 32)"
object_store_user="sb$(openssl rand -hex 12)"
object_store_password="$(openssl rand -hex 32)"
bootstrap_admin_token="$(openssl rand -hex 32)"
api_key_hash_secret="$(openssl rand -hex 48)"
runner_enrollment_hash_secret="$(openssl rand -hex 48)"
environment_directory="$(cd "$(dirname "$environment_path")" && pwd)"
environment_basename="$(basename "$environment_path")"
runner_pki_directory="$environment_directory/${environment_basename}.secrets/runner-pki"

if [[ -L "$runner_pki_directory" ]]; then
  echo "Refusing symbolic-link runner PKI directory: $runner_pki_directory" >&2
  exit 1
fi
if [[ -e "$runner_pki_directory" ]]; then
  echo "Refusing to replace an existing runner PKI directory: $runner_pki_directory" >&2
  exit 1
fi
install -d -m 700 "$runner_pki_directory"
openssl genpkey \
  -algorithm RSA \
  -pkeyopt rsa_keygen_bits:3072 \
  -out "$runner_pki_directory/runner-ca.key" 2>/dev/null
openssl req \
  -new \
  -x509 \
  -sha256 \
  -days 3650 \
  -key "$runner_pki_directory/runner-ca.key" \
  -subj "/CN=SecondBox Runner CA" \
  -out "$runner_pki_directory/runner-ca.crt"
openssl genpkey \
  -algorithm RSA \
  -pkeyopt rsa_keygen_bits:3072 \
  -out "$runner_pki_directory/server.key" 2>/dev/null
openssl req \
  -new \
  -sha256 \
  -key "$runner_pki_directory/server.key" \
  -subj "/CN=$runner_server_name" \
  -out "$runner_pki_directory/server.csr"
printf '%s\n' \
  'basicConstraints=critical,CA:FALSE' \
  'keyUsage=critical,digitalSignature,keyEncipherment' \
  'extendedKeyUsage=serverAuth' \
  "subjectAltName=$runner_server_san" \
  >"$runner_pki_directory/server.ext"
openssl x509 \
  -req \
  -sha256 \
  -days 825 \
  -in "$runner_pki_directory/server.csr" \
  -CA "$runner_pki_directory/runner-ca.crt" \
  -CAkey "$runner_pki_directory/runner-ca.key" \
  -CAcreateserial \
  -extfile "$runner_pki_directory/server.ext" \
  -out "$runner_pki_directory/server.crt" 2>/dev/null
rm -f \
  "$runner_pki_directory/server.csr" \
  "$runner_pki_directory/server.ext" \
  "$runner_pki_directory/runner-ca.srl"
chmod 600 "$runner_pki_directory/runner-ca.key" "$runner_pki_directory/server.key"
chmod 644 "$runner_pki_directory/runner-ca.crt" "$runner_pki_directory/server.crt"

temporary_path="$(mktemp "${environment_path}.tmp.XXXXXX")"
trap 'rm -f "$temporary_path"; rm -rf "$runner_pki_directory"' EXIT

awk \
  -v postgres_password="$postgres_password" \
  -v object_store_user="$object_store_user" \
  -v object_store_password="$object_store_password" \
  -v bootstrap_admin_token="$bootstrap_admin_token" \
  -v api_key_hash_secret="$api_key_hash_secret" \
  -v runner_enrollment_hash_secret="$runner_enrollment_hash_secret" \
  -v runner_pki_directory="$runner_pki_directory" '
  /^SECONDBOX_POSTGRES_PASSWORD=/ {
    print "SECONDBOX_POSTGRES_PASSWORD=" postgres_password
    next
  }
  /^SECONDBOX_DATABASE_URL=/ {
    print "SECONDBOX_DATABASE_URL=postgres://secondbox:" postgres_password "@postgres:5432/secondbox?sslmode=disable"
    next
  }
  /^SECONDBOX_OBJECT_STORE_ROOT_USER=/ {
    print "SECONDBOX_OBJECT_STORE_ROOT_USER=" object_store_user
    next
  }
  /^SECONDBOX_OBJECT_STORE_ROOT_PASSWORD=/ {
    print "SECONDBOX_OBJECT_STORE_ROOT_PASSWORD=" object_store_password
    next
  }
  /^SECONDBOX_BOOTSTRAP_ADMIN_TOKEN=/ {
    print "SECONDBOX_BOOTSTRAP_ADMIN_TOKEN=" bootstrap_admin_token
    next
  }
  /^SECONDBOX_API_KEY_HASH_SECRET=/ {
    print "SECONDBOX_API_KEY_HASH_SECRET=" api_key_hash_secret
    next
  }
  /^SECONDBOX_RUNNER_ENROLLMENT_HASH_SECRET=/ {
    print "SECONDBOX_RUNNER_ENROLLMENT_HASH_SECRET=" runner_enrollment_hash_secret
    next
  }
  /^SECONDBOX_RUNNER_PKI_HOST_DIR=/ {
    print "SECONDBOX_RUNNER_PKI_HOST_DIR=" runner_pki_directory
    next
  }
  { print }
  ' "$environment_path" >"$temporary_path"

chmod 600 "$temporary_path"
mv "$temporary_path" "$environment_path"
trap - EXIT
echo "Generated unique deployment credentials and runner PKI in private paths"
