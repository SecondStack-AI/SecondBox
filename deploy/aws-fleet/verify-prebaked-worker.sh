#!/usr/bin/env bash
set -euo pipefail
[[ $# == 2 ]] || { echo 'Usage: verify-prebaked-worker.sh CACHE_DIRECTORY EXPECTED_BUNDLE_SHA256' >&2; exit 2; }
cache=$1 expected=$2
[[ $cache == /* && $expected =~ ^[a-f0-9]{64}$ ]] || { echo 'SecondBox pre-baked cache requires an absolute directory and SHA256' >&2; exit 2; }
[[ -f $cache/prebaked.bundle.sha256 && $(cat "$cache/prebaked.bundle.sha256") == "$expected" ]] || {
  echo 'SecondBox pre-baked cache does not match the requested worker bundle' >&2; exit 1;
}
for asset in install-worker.sh stage-worker-execution-cache.sh bin/secondbox-fleet-enroll bin/secondbox-flat-root-digest runtime.env compose.yaml enrollment-ca.pem artifacts/materialization.json worker.tar.gz; do
  [[ -f $cache/$asset ]] || { echo "SecondBox pre-baked cache is missing $asset" >&2; exit 1; }
done
for executable in install-worker.sh stage-worker-execution-cache.sh bin/secondbox-fleet-enroll bin/secondbox-flat-root-digest; do
  [[ -x $cache/$executable ]] || { echo "SecondBox pre-baked cache cannot execute $executable" >&2; exit 1; }
done
[[ -d $cache/artifacts/rootfs ]] || { echo 'SecondBox pre-baked cache has no base root' >&2; exit 1; }
[[ ! -e $cache/execution-images ]] || { echo 'SecondBox pre-baked cache requires compressed execution-image staging' >&2; exit 1; }
if [[ -e $cache/prebaked.execution-images ]]; then
  [[ $(cat "$cache/prebaked.execution-images") == archive ]] || { echo 'SecondBox pre-baked execution-image marker is invalid' >&2; exit 1; }
fi
# A prepared cache contains immutable release inputs, never per-worker enrollment output.
for identity in runner.env runner.crt runner.key gateway.key enrollment.json; do
  [[ ! -e $cache/$identity ]] || { echo 'SecondBox pre-baked cache contains a worker identity' >&2; exit 1; }
done
for variable in SECONDBOX_RUNNER_IMAGE AGENT_RUNNER_GATEWAY_IMAGE; do
  mapfile -t values < <(sed -n "s/^$variable=//p" "$cache/runtime.env")
  [[ ${#values[@]} == 1 && ${values[0]} =~ ^sha256:[a-f0-9]{64}$ ]] || {
    echo "SecondBox pre-baked cache requires one immutable $variable" >&2; exit 1;
  }
  actual=$(docker image inspect --format '{{.Id}}' "${values[0]}")
  [[ $actual == "${values[0]}" ]] || { echo "SecondBox pre-baked Docker image differs: $variable" >&2; exit 1; }
done
printf 'SecondBox pre-baked worker cache verified: %s\n' "$expected"
