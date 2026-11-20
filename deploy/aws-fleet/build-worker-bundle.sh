#!/usr/bin/env bash
set -euo pipefail
umask 077
[[ $# == 6 || $# == 7 ]] || { echo 'Usage: build-worker-bundle.sh RUNNER_IMAGE GATEWAY_IMAGE MATERIALIZED_GVISOR_DIR ENROLLMENT_CA ENROLLMENT_BINARY OUTPUT_DIR [PREWARMED_EXECUTION_IMAGE_CACHE]' >&2; exit 2; }
runner_image=$1 gateway_image=$2 artifacts=$3 enrollment_ca=$4 enrollment_binary=$5 output=$6
[[ $(id -u) == 0 ]] || { echo 'Worker bundle requires root to preserve artifact numeric owners, modes and xattrs' >&2; exit 1; }
[[ $(tar --version) == *'GNU tar'* ]] || { echo 'Worker bundle requires GNU tar with pax, xattr and ACL support' >&2; exit 1; }
[[ $output == /* && $output != /tmp/* && $output != /run/* && $output != /dev/shm/* ]] || { echo 'Worker bundle output must be an absolute persistent directory' >&2; exit 1; }
[[ ! -e $output && ! -e $output.tar.gz ]] || { echo 'Worker bundle output already exists' >&2; exit 1; }
component=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$component/../.." && pwd)
test -d "$artifacts/rootfs"
test -f "$artifacts/materialization.json"
test -f "$enrollment_ca"
test -x "$enrollment_binary"
"$enrollment_binary" --version
mkdir -p "$output/bin" "$output/tmp"
export TMPDIR="$output/tmp"
install -m 0755 "$enrollment_binary" "$output/bin/secondbox-fleet-enroll"
(cd "$repo/runner" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$output/bin/secondbox-flat-root-digest" ./cmd/secondbox-flat-root-digest)
"$output/bin/secondbox-flat-root-digest" "$artifacts/rootfs" "$artifacts/materialization.json"
runner_id=$(docker image inspect --format '{{.Id}}' "$runner_image")
gateway_id=$(docker image inspect --format '{{.Id}}' "$gateway_image")
[[ $runner_id == sha256:* && $gateway_id == sha256:* ]] || { echo 'Worker image identities are absent' >&2; exit 1; }
docker image save --output "$output/worker-images.tar" "$runner_image" "$gateway_image"
mkdir -p "$output/artifacts"
tar --create --format=pax --numeric-owner --xattrs --xattrs-include='*' --acls --directory "$artifacts" . |
  tar --extract --same-owner --same-permissions --numeric-owner --xattrs --xattrs-include='*' --acls --delay-directory-restore --directory "$output/artifacts"
cp "$enrollment_ca" "$output/enrollment-ca.pem"
cp "$component/install-worker.sh" "$output/install-worker.sh"
cp "$component/verify-prebaked-worker.sh" "$output/verify-prebaked-worker.sh"
chmod 0755 "$output/install-worker.sh" "$output/verify-prebaked-worker.sh"
cp "$component/compose.yaml" "$output/compose.yaml"
printf 'SECONDBOX_RUNNER_IMAGE=%s\nAGENT_RUNNER_GATEWAY_IMAGE=%s\nSECONDBOX_GVISOR_RUNNER_CGROUP_PARENT=secondbox-elastic-worker-gvisor.slice\n' "$runner_id" "$gateway_id" > "$output/runtime.env"
bundle_files=(bin artifacts enrollment-ca.pem install-worker.sh verify-prebaked-worker.sh worker-images.tar compose.yaml runtime.env)
if [[ $# == 7 ]]; then
  test -d "$7"
  cp -a "$7" "$output/execution-images"
  rm -rf "$output/execution-images/tmp"
  bundle_files+=(execution-images)
fi
tar --create --gzip --format=pax --numeric-owner --xattrs --xattrs-include='*' --acls --file "$output.tar.gz" --directory "$output" "${bundle_files[@]}"
roundtrip="$output/roundtrip"
mkdir -p "$roundtrip"
tar --extract --gzip --same-owner --same-permissions --numeric-owner --xattrs --xattrs-include='*' --acls --delay-directory-restore --file "$output.tar.gz" --directory "$roundtrip" artifacts bin/secondbox-flat-root-digest
"$roundtrip/bin/secondbox-flat-root-digest" "$roundtrip/artifacts/rootfs" "$roundtrip/artifacts/materialization.json"
rm -rf "$roundtrip"
sha256sum "$output.tar.gz"
