#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
  echo "Usage: deploy/bin/prepare-development-inventory.sh PATH" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
environment_path="$1"
compose_path="$repo_root/deploy/compose.yml"

if [[ -L "$environment_path" || ! -f "$environment_path" ]]; then
  echo "SecondBox development inventory requires a regular non-symbolic-link environment file: $environment_path" >&2
  exit 1
fi
for required_command in awk docker; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "Development inventory preparation requires command: $required_command" >&2
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
  echo "Development inventory preparation requires exactly one SECONDBOX_DEPLOYMENT_MODE" >&2
  exit 1
}
if [[ "$deployment_mode" != "development" ]]; then
  echo "Development inventory preparation requires SECONDBOX_DEPLOYMENT_MODE=development" >&2
  exit 1
fi

"$repo_root/deploy/bin/validate-environment.sh" "$environment_path"
wait_timeout_seconds="$(
  environment_value SECONDBOX_DEVELOPMENT_PREPARE_WAIT_TIMEOUT_SECONDS
)" || {
  echo "Development inventory preparation requires exactly one SECONDBOX_DEVELOPMENT_PREPARE_WAIT_TIMEOUT_SECONDS" >&2
  exit 1
}
docker compose \
  --env-file "$environment_path" \
  --file "$compose_path" \
  --profile development \
  up \
  --detach \
  --wait \
  --wait-timeout "$wait_timeout_seconds" \
  postgres \
  object-store
docker compose \
  --env-file "$environment_path" \
  --file "$compose_path" \
  --profile development \
  run \
  --rm \
  --no-deps \
  object-store-init

echo "SecondBox development PostgreSQL, RustFS, and bucket inventory are prepared"
