#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "SecondBox PostgreSQL client installation must run as root" >&2
  exit 1
fi

source /etc/os-release
: "${VERSION_CODENAME:?Linux distribution codename is unavailable}"

architecture="$(dpkg --print-architecture)"
case "$architecture" in
  amd64|arm64) ;;
  *)
    echo "SecondBox PostgreSQL client architecture is unsupported: $architecture" >&2
    exit 1
    ;;
esac

key_url="https://www.postgresql.org/media/keys/ACCC4CF8.asc"
key_sha256="0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76"
key_directory="/usr/share/postgresql-common/pgdg"
key_path="$key_directory/apt.postgresql.org.asc"
source_path="/etc/apt/sources.list.d/pgdg.sources"

work_directory="$(mktemp -d)"
cleanup() {
  rm -rf -- "$work_directory"
}
trap cleanup EXIT

apt-get update
apt-get install -y --no-install-recommends ca-certificates curl
curl \
  --fail \
  --location \
  --proto '=https' \
  --tlsv1.2 \
  --output "$work_directory/apt.postgresql.org.asc" \
  "$key_url"
printf '%s  %s\n' \
  "$key_sha256" \
  "$work_directory/apt.postgresql.org.asc" |
  sha256sum --check --strict

install -d -m 0755 "$key_directory"
install -m 0644 "$work_directory/apt.postgresql.org.asc" "$key_path"
printf '%s\n' \
  "Types: deb" \
  "URIs: https://apt.postgresql.org/pub/repos/apt" \
  "Suites: ${VERSION_CODENAME}-pgdg" \
  "Architectures: ${architecture}" \
  "Components: main" \
  "Signed-By: ${key_path}" \
  >"$source_path"

apt-get update
apt-get install -y --no-install-recommends postgresql-client-18

case "$(/usr/lib/postgresql/18/bin/pg_dump --version)" in
  "pg_dump (PostgreSQL) 18."*) ;;
  *)
    echo "SecondBox pg_dump installation did not provide PostgreSQL 18" >&2
    exit 1
    ;;
esac
