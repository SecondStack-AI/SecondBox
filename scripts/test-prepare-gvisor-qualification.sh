#!/usr/bin/env bash
set -Eeuo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf -- "$temporary"' EXIT
mkdir -p "$temporary/repo/scripts" "$temporary/repo/runner/scripts" "$temporary/bin" "$temporary/assets/bin" "$temporary/assets/rootfs"
cp "$repo_root/scripts/prepare-gvisor-qualification.sh" "$temporary/repo/scripts/"
cp "$repo_root/scripts/qualify-gvisor.sh" "$temporary/repo/scripts/"
export TEST_ASSETS="$temporary/assets" TEST_CALLS="$temporary/calls"
export TEST_IMAGE="sha256:$(printf image | sha256sum | cut -d' ' -f1)"
export QUALIFY_GVISOR_HOST_BUILD_ROOT="$temporary/cache"
export PATH="$temporary/bin:$PATH"
printf 'rootfs\n' >"$TEST_ASSETS/rootfs/content"
for name in runsc secondbox-guest-agent secondbox-flat-root-digest secondbox-materialization-digest; do
  printf '#!/bin/sh\nexit 0\n' >"$TEST_ASSETS/bin/$name"
  chmod +x "$TEST_ASSETS/bin/$name"
done
printf 'readonly RUNSC_SHA512="%s"\n' "$(sha512sum "$TEST_ASSETS/bin/runsc" | cut -d' ' -f1)" >"$temporary/repo/runner/scripts/fetch-runsc.sh"
cat >"$temporary/bin/secondbox-flat-root-digest" <<'DIGEST'
#!/usr/bin/env bash
set -euo pipefail
[[ "${TEST_FAIL_VERIFY:-0}" != 1 ]] || exit 91
sha256sum "$1/content" | cut -d' ' -f1
DIGEST
cat >"$temporary/bin/secondbox-materialization-digest" <<'DIGEST'
#!/usr/bin/env bash
sha256sum "$1" | cut -d' ' -f1
DIGEST
chmod +x "$temporary/bin/secondbox-"*
printf '{"key":{"runtimeManifestDigest":"runtime","toolchainManifestDigest":"toolchain"}}\n' >"$TEST_ASSETS/materialization.json"
jq -n --arg flatRootDigest "$(secondbox-flat-root-digest "$TEST_ASSETS/rootfs")" --arg materializationDigest "$(secondbox-materialization-digest "$TEST_ASSETS/materialization.json")" \
  '{flatRootDigest:$flatRootDigest,materializationDigest:$materializationDigest}' >"$TEST_ASSETS/identity.json"
(cd "$TEST_ASSETS" && sha256sum bin/* materialization.json identity.json >SHA256SUMS)
cat >"$temporary/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
case "$1 ${2:-}" in
  'info '|'buildx version') exit 0 ;;
  'buildx build')
    echo build >>"$TEST_CALLS"
    while (($#)); do
      if [[ "$1" == --iidfile ]]; then printf '%s\n' "$TEST_IMAGE" >"$2"; break; fi
      shift
    done
    ;;
  'run --rm')
    while [[ "$1" != -- ]]; do shift; done
    shift
    body="$(cat)"
    body="${body//\/secondbox-runner-gvisor/$TEST_ASSETS}"
    body="${body//\/output/$QUALIFY_GVISOR_HOST_BUILD_ROOT}"
    # Execute the real extraction/verification transaction with fixture assets.
    bash -eu -s -- "$@" <<<"$body"
    ;;
  *) echo "unexpected Docker call: $*" >&2; exit 1 ;;
esac
DOCKER
chmod +x "$temporary/bin/docker"
cd "$temporary/repo"
git init -q
git -c user.name=Qualification -c user.email=qualification@example.invalid add .
git -c user.name=Qualification -c user.email=qualification@example.invalid commit -qm fixture
prepare=scripts/prepare-gvisor-qualification.sh
expect_failure() {
  if "$@" >"$temporary/failure.log" 2>&1; then echo "unexpected success: $*" >&2; exit 1; fi
}

# Missing-root preflight succeeds without downloading or creating a directory.
"$prepare" --preflight
[[ ! -e "$QUALIFY_GVISOR_HOST_BUILD_ROOT" && ! -e "$TEST_CALLS" ]]
expect_failure env QUALIFY_GVISOR_HOST_BUILD_ROOT=relative "$prepare" --preflight
mkdir "$temporary/unmanaged"
printf keep >"$temporary/unmanaged/operator-file"
expect_failure env QUALIFY_GVISOR_HOST_BUILD_ROOT="$temporary/unmanaged" "$prepare"
[[ "$(cat "$temporary/unmanaged/operator-file")" == keep && ! -e "$temporary/unmanaged/prepare.lock" ]]
ln -s "$temporary/unmanaged" "$temporary/link"
expect_failure env QUALIFY_GVISOR_HOST_BUILD_ROOT="$temporary/link/cache" "$prepare"

# Failed verification never publishes a build, and retry completes normally.
expect_failure env TEST_FAIL_VERIFY=1 "$prepare"
[[ ! -e "$QUALIFY_GVISOR_HOST_BUILD_ROOT/${TEST_IMAGE#sha256:}" ]]
[[ -z "$(find "$QUALIFY_GVISOR_HOST_BUILD_ROOT" -name '.extract.*')" ]]
"$prepare" >"$temporary/first"
build="$(cat "$temporary/first")"
before="$(stat -c '%i:%Y' "$build/bin/secondbox-guest-agent")"
"$prepare" >"$temporary/repeat"
cmp "$temporary/first" "$temporary/repeat"
[[ "$before" == "$(stat -c '%i:%Y' "$build/bin/secondbox-guest-agent")" ]]

# Host corruption cannot be blessed by editing the cached checksum manifest.
printf corrupt >>"$build/rootfs/content"
expect_failure "$prepare"
cp "$TEST_ASSETS/rootfs/content" "$build/rootfs/content"
printf corrupt >>"$build/bin/runsc"
(cd "$build" && sha256sum bin/* materialization.json identity.json >SHA256SUMS)
expect_failure "$prepare"
cp -a "$TEST_ASSETS/bin/runsc" "$build/bin/runsc"
cp "$TEST_ASSETS/SHA256SUMS" "$build/SHA256SUMS"

# A new build leaves the old reader's bytes in place. Concurrent first callers
# publish one complete directory and both select it without replacing inodes.
export TEST_IMAGE="sha256:$(printf new-image | sha256sum | cut -d' ' -f1)"
"$prepare" >"$temporary/new"
[[ "$(cat "$temporary/new")" != "$build" && -d "$build" ]]
[[ "$before" == "$(stat -c '%i:%Y' "$build/bin/secondbox-guest-agent")" ]]
export QUALIFY_GVISOR_HOST_BUILD_ROOT="$temporary/concurrent"
"$prepare" >"$temporary/a" 2>"$temporary/a.log" & a=$!
"$prepare" >"$temporary/b" 2>"$temporary/b.log" & b=$!
wait "$a"; wait "$b"
cmp "$temporary/a" "$temporary/b"
[[ "$before" == "$(stat -c '%i:%Y' "$build/bin/secondbox-guest-agent")" ]]
[[ "$(find "$QUALIFY_GVISOR_HOST_BUILD_ROOT" -mindepth 1 -maxdepth 1 -type d | wc -l)" == 1 ]]

# Exercise the actual --host dispatch through preparation to the scenario
# boundary; host mechanisms themselves are exercised by real qualification.
cat >"$temporary/bin/findmnt" <<'FINDMNT'
#!/usr/bin/env bash
echo btrfs
FINDMNT
cat >scripts/test-scenario.sh <<'SCENARIO'
#!/usr/bin/env bash
set -euo pipefail
[[ "$SECONDBOX_SCENARIO_COMPUTE_BACKEND" == gvisor ]]
[[ "$SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST" == runtime ]]
[[ "$SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST" == toolchain ]]
[[ "$SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION" == "$SECONDBOX_SCENARIO_GVISOR_BUILD/materialization.json" ]]
[[ "$SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION_DIGEST" == "$(jq -er .materializationDigest "$SECONDBOX_SCENARIO_GVISOR_BUILD/identity.json")" ]]
[[ "$SECONDBOX_SCENARIO_ARTIFACT_MANIFEST_DIGEST" == "$SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION_DIGEST" ]]
SCENARIO
chmod +x "$temporary/bin/findmnt" scripts/test-scenario.sh
export QUALIFY_GVISOR_HOST_BUILD_ROOT="$temporary/dispatch"
export SECONDBOX_RUNNER_WORKSPACE_ROOT="$temporary/workspaces"
mkdir "$SECONDBOX_RUNNER_WORKSPACE_ROOT"
scripts/qualify-gvisor.sh --host --preflight
[[ ! -e "$QUALIFY_GVISOR_HOST_BUILD_ROOT" ]]
scripts/qualify-gvisor.sh --host
echo 'SecondBox gVisor preparation tests passed'
