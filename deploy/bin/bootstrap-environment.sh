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

if ! grep -Eq 'GENERATE_WITH_DEPLOY_BOOTSTRAP|GENERATE_LOCAL_DATABASE_URL|GENERATE_RUNNER_PKI|GENERATE_DEVELOPMENT_ASSET_CATALOG' "$environment_path"; then
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

environment_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $1 == key {
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
}

deployment_mode="$(environment_value SECONDBOX_DEPLOYMENT_MODE)" || {
  echo "Bootstrap requires exactly one SECONDBOX_DEPLOYMENT_MODE" >&2
  exit 1
}
signed_asset_catalog_host_path="$(
  environment_value SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH
)" || {
  echo "Bootstrap requires exactly one SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH" >&2
  exit 1
}
if [[ "$deployment_mode" != "development" && "$deployment_mode" != "production" ]]; then
  echo "SECONDBOX_DEPLOYMENT_MODE must be development or production" >&2
  exit 1
fi
if [[ "$deployment_mode" == "production" &&
      "$signed_asset_catalog_host_path" == "GENERATE_DEVELOPMENT_ASSET_CATALOG" ]]; then
  echo "SecondBox production requires an operator-supplied SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH" >&2
  exit 1
fi

runner_server_name="$(
  environment_value SECONDBOX_RUNNER_SERVER_NAME
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
development_asset_catalog_path="$environment_directory/${environment_basename}.secrets/development-signed-assets.json"
temporary_path=""
runner_pki_created=false
development_asset_catalog_created=false

cleanup_incomplete_bootstrap() {
  local status="$?"
  trap - EXIT
  if [[ -n "$temporary_path" ]]; then
    rm -f -- "$temporary_path"
  fi
  if [[ "$development_asset_catalog_created" == "true" ]]; then
    rm -f -- "$development_asset_catalog_path"
  fi
  if [[ "$runner_pki_created" == "true" ]]; then
    rm -rf -- "$runner_pki_directory"
  fi
  exit "$status"
}
trap cleanup_incomplete_bootstrap EXIT

if [[ -L "$runner_pki_directory" ]]; then
  echo "Refusing symbolic-link runner PKI directory: $runner_pki_directory" >&2
  exit 1
fi
if [[ -e "$runner_pki_directory" ]]; then
  echo "Refusing to replace an existing runner PKI directory: $runner_pki_directory" >&2
  exit 1
fi
install -d -m 700 "$runner_pki_directory"
runner_pki_created=true
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

if [[ "$signed_asset_catalog_host_path" == "GENERATE_DEVELOPMENT_ASSET_CATALOG" ]]; then
  if [[ -L "$development_asset_catalog_path" || -e "$development_asset_catalog_path" ]]; then
    echo "Refusing to replace an existing development asset catalog: $development_asset_catalog_path" >&2
    exit 1
  fi
  install -m 644 /dev/null "$development_asset_catalog_path"
  development_asset_catalog_created=true
  printf '%s\n' \
    '{' \
    '  "assets": [' \
    '    {' \
    '      "artifactId": "secondbox-development-bootstrap",' \
    '      "manifestDigest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",' \
    '      "signatureKeyId": "secondbox-development-local-trust",' \
    '      "architecture": "amd64",' \
    '      "guestProtocolGeneration": 1,' \
    '      "mandatoryGuestFeatures": []' \
    '    }' \
    '  ]' \
    '}' >"$development_asset_catalog_path"
  chmod 644 "$development_asset_catalog_path"
  signed_asset_catalog_host_path="$development_asset_catalog_path"
fi

temporary_path="$(mktemp "${environment_path}.tmp.XXXXXX")"

awk \
  -v postgres_password="$postgres_password" \
  -v object_store_user="$object_store_user" \
  -v object_store_password="$object_store_password" \
  -v bootstrap_admin_token="$bootstrap_admin_token" \
  -v api_key_hash_secret="$api_key_hash_secret" \
  -v runner_enrollment_hash_secret="$runner_enrollment_hash_secret" \
  -v runner_pki_directory="$runner_pki_directory" \
  -v signed_asset_catalog_host_path="$signed_asset_catalog_host_path" '
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
  /^SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH=/ {
    print "SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH=" signed_asset_catalog_host_path
    next
  }
  { print }
  ' "$environment_path" >"$temporary_path"

chmod 600 "$temporary_path"
mv "$temporary_path" "$environment_path"
trap - EXIT
if [[ "$development_asset_catalog_created" == "true" ]]; then
  echo "Generated unique deployment credentials, development inventory, and runner PKI in private paths"
else
  echo "Generated unique deployment credentials and runner PKI in private paths"
fi
