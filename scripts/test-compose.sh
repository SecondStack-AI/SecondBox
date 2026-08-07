#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose_file="$repo_root/scripts/compose-test.yml"
binary_dir="$repo_root/.tmp/compose-test"
project_name="secondbox-compose-test-$$"

for command in curl docker go node openssl; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "SecondBox Compose test prerequisite missing: $command" >&2
    exit 1
  fi
done
if ! docker compose version >/dev/null 2>&1; then
  echo "SecondBox Compose test prerequisite missing: Docker Compose v2" >&2
  exit 1
fi

mkdir -p "$binary_dir"
cd "$repo_root"
CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -o "$binary_dir/secondboxd" ./cmd/secondboxd
chmod 0755 "$binary_dir/secondboxd"
# The Go live half is compiled before the fixture Runner is stamped alive so that
# compilation never consumes the fixture's heartbeat window.
go_live_test="$binary_dir/sdk-live.test"
go test -c -tags=sdk_live -o "$go_live_test" ./tests/sdk-live

pki_dir="$(mktemp -d "$binary_dir/runner-pki.XXXXXX")"
export SECONDBOX_COMPOSE_TEST_PKI_DIR="$pki_dir"
export SECONDBOX_COMPOSE_TEST_UID
SECONDBOX_COMPOSE_TEST_UID="$(id -u)"
export SECONDBOX_COMPOSE_TEST_GID
SECONDBOX_COMPOSE_TEST_GID="$(id -g)"

openssl req -x509 -newkey rsa:3072 -nodes \
  -keyout "$pki_dir/runner-ca.key" \
  -out "$pki_dir/runner-ca.crt" \
  -days 2 \
  -subj "/CN=SecondBox Compose Runner CA" >/dev/null 2>&1
openssl req -new -newkey rsa:3072 -nodes \
  -keyout "$pki_dir/server.key" \
  -out "$pki_dir/server.csr" \
  -subj "/CN=control-plane" >/dev/null 2>&1
printf '%s\n' \
  "subjectAltName=DNS:control-plane,DNS:localhost,IP:127.0.0.1" \
  "extendedKeyUsage=serverAuth" >"$pki_dir/server.ext"
openssl x509 -req \
  -in "$pki_dir/server.csr" \
  -CA "$pki_dir/runner-ca.crt" \
  -CAkey "$pki_dir/runner-ca.key" \
  -CAcreateserial \
  -out "$pki_dir/server.crt" \
  -days 2 \
  -extfile "$pki_dir/server.ext" >/dev/null 2>&1
chmod 0600 "$pki_dir/runner-ca.key" "$pki_dir/server.key"
chmod 0644 "$pki_dir/runner-ca.crt" "$pki_dir/server.crt"

compose() {
  docker compose --project-name "$project_name" --file "$compose_file" "$@"
}

cleanup() {
  status="$?"
  trap - EXIT
  if [[ "$status" -ne 0 ]]; then
    if ! compose logs control-plane postgres >&2; then
      echo "SecondBox Compose smoke test could not collect failure logs" >&2
    fi
  fi
  if ! compose down --volumes --remove-orphans >/dev/null 2>&1; then
    echo "SecondBox Compose smoke test cleanup failed for project $project_name" >&2
    if [[ "$status" -eq 0 ]]; then
      status=1
    fi
  fi
  if ! rm -r -- "$pki_dir"; then
    echo "SecondBox Compose smoke test PKI cleanup failed: $pki_dir" >&2
    if [[ "$status" -eq 0 ]]; then
      status=1
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

compose config --quiet
compose up --detach --wait --wait-timeout 180

health_status="$(compose ps --format json control-plane)"
if ! grep -q '"Health":"healthy"' <<<"$health_status"; then
  compose logs control-plane postgres >&2
  echo "SecondBox Compose smoke test failed: control plane did not become healthy" >&2
  exit 1
fi

published_address="$(compose port control-plane 8080)"
if [[ "$published_address" != 127.0.0.1:* ]]; then
  echo "SecondBox Compose smoke test failed: API port is not loopback-only: $published_address" >&2
  exit 1
fi
live_base_url="http://$published_address"

health_body="$(curl --fail --silent --show-error "$live_base_url/healthz")"
if [[ "$health_body" != '{"status":"healthy"}' ]]; then
  echo "SecondBox Compose smoke test failed: unexpected health response: $health_body" >&2
  exit 1
fi
readiness_body="$(curl --fail --silent --show-error "$live_base_url/readyz")"
if [[ "$readiness_body" != '{"status":"ready"}' ]]; then
  echo "SecondBox Compose smoke test failed: unexpected readiness response: $readiness_body" >&2
  exit 1
fi

if ! compose exec -T postgres psql \
  --username secondbox \
  --dbname secondbox_compose_test \
  --set ON_ERROR_STOP=on \
  --quiet <"$repo_root/scripts/compose-test-init.sql" >/dev/null; then
  echo "SecondBox Compose smoke test failed: live fixture insert failed" >&2
  exit 1
fi

refresh_fixture_runner() {
  local half="$1"
  local refreshed
  if ! refreshed="$(compose exec -T postgres psql \
    --username secondbox \
    --dbname secondbox_compose_test \
    --set ON_ERROR_STOP=on \
    --quiet --tuples-only --no-align <"$repo_root/scripts/compose-test-runner-heartbeat.sql")"; then
    echo "SecondBox Compose smoke test failed: fixture Runner heartbeat refresh failed before the $half live half" >&2
    exit 1
  fi
  if [[ "$refreshed" != "compose-live-runner" ]]; then
    echo "SecondBox Compose smoke test failed: fixture Runner heartbeat refresh matched no Runner before the $half live half: $refreshed" >&2
    exit 1
  fi
}

export SECONDBOX_LIVE_BASE_URL="$live_base_url"
export SECONDBOX_LIVE_PLATFORM_TOKEN="compose-test-platform-token-0000000000000000"
refresh_fixture_runner Go
"$go_live_test" -test.v
refresh_fixture_runner TypeScript
node --test tests/sdk-live/typescript_live.test.ts

echo "SecondBox Compose smoke test passed: PostgreSQL readiness and live Go/TypeScript SDK contracts"
