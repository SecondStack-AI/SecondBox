#!/usr/bin/env bash
set -Eeuo pipefail
caller_umask="$(umask)"
umask 077
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
fail() { echo "SecondBox qualification: $*" >&2; exit 1; }

wait_run() {
  local run="$1" status
  [[ "$run" =~ ^[0-9]{8}T[0-9]{6}-[0-9]+$ ]] || fail "invalid run ID"
  local directory="$repo_root/.tmp/qualify/$run"
  [[ -f "$directory/pid" ]] || fail "unknown run: $run"
  echo "Logs: $directory (tail -F $directory/*.log)"
  while [[ ! -f "$directory/result" ]]; do
    if ! kill -0 "$(cat "$directory/pid")" 2>/dev/null; then
      [[ -f "$directory/result" ]] || fail "supervisor stopped without a result; inspect $directory/supervisor.log"
    fi
    sleep 2
  done
  cat "$directory/timing.md"
  status="$(cat "$directory/result")"
  return "$status"
}

scenario_shards() {
  local backend="$1" count="$QUALIFY_FIRECRACKER_SHARDS" status=0
  local shard_root="$directory/$backend-shards"
  mkdir -p "$shard_root"
  if [[ "$tier" != pr ]]; then
    if [[ "$backend" == firecracker ]]; then rm -f .tmp/scenario-qualification-evidence.json
    else rm -f .tmp/gvisor-linux-scenario-qualification-evidence.json; fi
  fi
  local -a shard_jobs=() command=(just test-scenario)
  [[ "$backend" != gvisor ]] || command=(scripts/qualify-gvisor.sh --host)
  for ((i=1; i<=count; i++)); do
    stack_stage "$backend-$i" env SECONDBOX_SCENARIO_SHARD="$i/$count" SECONDBOX_SCENARIO_SHARD_DIRECTORY="$shard_root" SECONDBOX_SCENARIO_COMPUTE_BACKEND="$backend" SECONDBOX_SCENARIO_RUNNER_PLACEMENT=compose SECONDBOX_SCENARIO_MODE=suite "${command[@]}" & shard_jobs+=("$!")
  done
  for job in "${shard_jobs[@]}"; do wait "$job" || status=1; done
  for ((i=1; i<=count; i++)); do
    IFS=$'\t' read -r name code elapsed <"$directory/$backend-$i.status"
    ((code == 0)) || status=1
  done
  ((status == 0)) || return 1
  # PRs may qualify a dirty working tree; only release/nightly publish a merge.
  if [[ "$tier" != pr ]]; then
    SECONDBOX_SCENARIO_COMPUTE_BACKEND="$backend" scripts/test-scenario.sh --merge-shards "$count" "$shard_root"
  fi
}

# Both backend schedulers share this pool. Keep a slot through teardown and
# serialize admission until the preceding control plane actually serves readyz.
stack_stage() (
  local name="$1"; shift
  local startup slot index job lock_status
  exec {startup}>"$directory/startup.lock"
  flock "$startup" || return
  while :; do
    for ((index=1; index<=QUALIFY_MAX_STACKS; index++)); do
      # Gates launch first; test reserves one slot until its status is published.
      if [[ ( "$only" == all || "$only" == gates ) && ! -f "$directory/test.status" ]]; then
        [[ "$QUALIFY_GATES_FIRST" == 0 && "$index" != 1 ]] || continue
      fi
      exec {slot}>"$directory/stack-$index.lock"
      if flock -n "$slot"; then break 2
      else lock_status=$?; fi
      exec {slot}>&-
      [[ "$lock_status" == 1 ]] || return "$lock_status"
    done
    sleep 0.1
  done
  echo "Admitted $name in slot $index"
  stage "$name" env SECONDBOX_SCENARIO_READY_FILE="$directory/$name.ready" "$@" & job=$!
  while [[ ! -f "$directory/$name.ready" ]] && kill -0 "$job" 2>/dev/null; do sleep 0.1; done
  flock -u "$startup"
  exec {startup}>&-
  wait "$job"
)

sdk_packages() {
  # Packaging consumes the declarations produced by verify-generated.
  while [[ ! -f "$directory/verify-generated.status" ]]; do sleep 1; done
  local name code elapsed
  IFS=$'\t' read -r name code elapsed <"$directory/verify-generated.status"
  ((code == 0)) || { echo "SecondBox SDK package gate requires successful generated validation"; return 1; }
  "${gate_env[@]}" just test-sdk-packages
}

stage() {
  local name="$1"; shift
  local start=$SECONDS status=0
  (umask "$caller_umask"; "$@") >"$directory/$name.log" 2>&1 || status=$?
  printf '%s\t%s\t%s\n' "$name" "$status" "$((SECONDS-start))" >"$directory/$name.status.tmp"
  mv "$directory/$name.status.tmp" "$directory/$name.status"
  return 0
}

# provision_test_database starts one disposable PostgreSQL container bound to a
# random loopback port for the Go suite gate and removes it when the worker
# exits. The password never leaves this process; the URL is exported to the
# stages only.
provision_test_database() {
  local run="$1" container port password
  container="secondbox-suite-qualify-pg-$run"
  password="$(head -c 24 /dev/urandom | base64 | tr -d '/+=')"
  docker run --detach --name "$container" --publish 127.0.0.1::5432 \
    --env POSTGRES_PASSWORD="$password" --env POSTGRES_DB=secondbox_integration \
    "$QUALIFY_TEST_POSTGRES_IMAGE" >/dev/null || fail 'could not start the disposable test PostgreSQL container'
  trap 'docker rm -f "'"$container"'" >/dev/null 2>&1 || true' EXIT
  port="$(docker port "$container" 5432/tcp | head -n1 | sed 's/.*://')"
  [[ "$port" =~ ^[0-9]+$ ]] || fail 'could not read the disposable test PostgreSQL port'
  local attempt
  for attempt in $(seq 1 60); do
    if docker exec "$container" pg_isready -U postgres -d secondbox_integration >/dev/null 2>&1 &&
       docker exec "$container" psql -U postgres -d secondbox_integration -Atc 'select 1' >/dev/null 2>&1; then
      export SECONDBOX_TEST_DATABASE_URL="postgres://postgres:$password@127.0.0.1:$port/secondbox_integration?sslmode=disable"
      echo "Disposable test PostgreSQL: $container on 127.0.0.1:$port"
      return 0
    fi
    sleep 1
  done
  fail 'disposable test PostgreSQL did not become ready within 60 s'
}

# gate_env strips scenario and Runner variables from gate stages; set in worker.
gate_env=(env)

worker() {
  directory="$1"
  # The environment file is private and generated by this command, never read from logs.
  source "$directory/environment"
  export QUALIFY_GVISOR_BUILD_ID="$directory"
  echo "$$" >"$directory/pid.tmp"
  mv "$directory/pid.tmp" "$directory/pid"
  exec 9>"$repo_root/.tmp/qualify/checkout.lock"
  flock -n 9 || fail "another qualification owns this checkout"
  local start=$SECONDS status=0 name code elapsed
  if [[ "$tier" != pr ]]; then
    [[ "$(git rev-parse HEAD)" == "$source_commit" && -z "$(git status --porcelain --untracked-files=all)" ]] || fail "release source changed after preflight"
  fi
  local -a jobs=()
  if [[ "$only" == all || "$only" == gates ]]; then
    if [[ "${QUALIFY_PROVISION_TEST_DB:-0}" == 1 ]]; then
      provision_test_database "$(basename "$directory")"
    fi
    # Cached diagnostics contain source paths; do not share them across worktrees.
    export GOLANGCI_LINT_CACHE="$repo_root/.tmp/golangci-lint"
    # Gates are unit and contract suites; the scenario and Runner variables from
    # the configuration file must not reach them (deployconfig tests derive
    # Runner paths from the environment and would see the operator's values).
    gate_env=(env)
    while IFS= read -r name; do gate_env+=(-u "$name"); done < <(compgen -e | grep -E '^SECONDBOX_(SCENARIO_|RUNNER_|REQUIRE_QUALIFIED_SCENARIO$)')
    for name in verify-generated test test-contract test-compose test-image-policy test-sdk-packages test-deployment test-install-docs test-release-workflow lint; do
      if [[ "$name" == test-sdk-packages ]]; then
        stage "$name" sdk_packages & jobs+=("$!")
      else
        stage "$name" "${gate_env[@]}" just "$name" & jobs+=("$!")
      fi
    done
  fi
  if [[ "$QUALIFY_GATES_FIRST" == 1 ]]; then
    for job in "${jobs[@]}"; do wait "$job" || status=1; done
    jobs=()
  fi
  if [[ "$only" == all || "$only" == firecracker ]]; then
    if [[ "$tier" == nightly ]]; then
      stack_stage firecracker env -u SECONDBOX_SCENARIO_SHARD SECONDBOX_SCENARIO_COMPUTE_BACKEND=firecracker just test-scenario & jobs+=("$!")
    else
      stage firecracker scenario_shards firecracker & jobs+=("$!")
    fi
  fi
  if [[ "$only" == gvisor || ( "$only" == all && "$tier" != pr ) ]]; then
    if [[ "$tier" == nightly ]]; then
      stack_stage gvisor-host scripts/qualify-gvisor.sh --host & jobs+=("$!")
      stack_stage gvisor scripts/qualify-gvisor.sh "$directory" "$source_commit" & jobs+=("$!")
    else
      stage gvisor-host scenario_shards gvisor & jobs+=("$!")
    fi
  fi
  for job in "${jobs[@]}"; do wait "$job" || status=1; done
  if [[ "$tier" != pr ]] && { [[ "$(git rev-parse HEAD)" != "$source_commit" ]] || [[ -n "$(git status --porcelain --untracked-files=all)" ]]; }; then
    printf 'source-integrity\t1\t0\n' >"$directory/source-integrity.status"
  fi
  {
    printf '| Stage | Result | Wall clock |\n|---|---|---|\n'
    for file in "$directory"/*.status; do
      IFS=$'\t' read -r name code elapsed <"$file"
      if ((code == 0)); then result=PASS; else result="FAIL ($code)"; status=1; fi
      printf '| %s | %s | %sm %ss |\n' "$name" "$result" "$((elapsed/60))" "$((elapsed%60))"
    done
    elapsed=$((SECONDS-start))
    printf '| Total | %s | %sm %ss |\n' "$([[ $status == 0 ]] && echo PASS || echo FAIL)" "$((elapsed/60))" "$((elapsed%60))"
  } >"$directory/timing.md"
  cat "$directory/timing.md"
  printf '%s\n' "$status" >"$directory/result.tmp"
  mv "$directory/result.tmp" "$directory/result"
}

if [[ "${1:-}" == --worker ]]; then worker "$2"; exit; fi
if [[ "${1:-}" == --wait ]]; then [[ $# == 2 ]] || fail 'usage: --wait RUN'; wait_run "$2"; exit; fi
tier=pr only=all preflight=false
while (($#)); do
  case "$1" in
    --preflight) preflight=true; shift ;;
    --tier|--only) [[ $# -ge 2 ]] || fail "missing value for $1"; printf -v "${1#--}" '%s' "$2"; shift 2 ;;
    --help) echo 'Usage: just qualify [--tier pr|release|nightly] [--only gates|firecracker|gvisor] | --wait RUN'; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
[[ "$tier" == pr || "$tier" == release || "$tier" == nightly ]] || fail 'tier must be pr, release, or nightly'
[[ "$only" == all || "$only" == gates || "$only" == firecracker || "$only" == gvisor ]] || fail 'only must be gates, firecracker, or gvisor'
config="${QUALIFY_ENV_FILE:-${HOME:?}/.config/secondbox/qualify.env}"
[[ -f "$config" ]] || fail "copy deploy/qualify.env.example to $config and configure it"
bash -n "$config" || fail "invalid environment file: $config"
set -a
source "$config"
set +a
unset SECONDBOX_SCENARIO_SHARD SECONDBOX_SCENARIO_TEST_PATTERN
export SECONDBOX_SCENARIO_MODE=suite SECONDBOX_SCENARIO_RUNNER_PLACEMENT=compose
export SECONDBOX_SCENARIO_TIER=release
[[ "$tier" != nightly ]] || export SECONDBOX_SCENARIO_TIER=nightly
export QUALIFY_FIRECRACKER_SHARDS="${QUALIFY_FIRECRACKER_SHARDS:-4}"
export QUALIFY_MAX_STACKS="${QUALIFY_MAX_STACKS:-4}"
export QUALIFY_GATES_FIRST="${QUALIFY_GATES_FIRST:-0}"
[[ "$QUALIFY_MAX_STACKS" =~ ^([1-9]|[1-5][0-9]|6[0-4])$ ]] || fail 'QUALIFY_MAX_STACKS must be 1..64'
[[ "$QUALIFY_GATES_FIRST" == 0 || "$QUALIFY_GATES_FIRST" == 1 ]] || fail 'QUALIFY_GATES_FIRST must be 0 or 1'
[[ "$QUALIFY_FIRECRACKER_SHARDS" =~ ^[1-9][0-9]*$ ]] && ((QUALIFY_FIRECRACKER_SHARDS <= 64)) || fail 'QUALIFY_FIRECRACKER_SHARDS must be 1..64'
for tool in git just go flock setsid; do command -v "$tool" >/dev/null || fail "missing tool: $tool"; done
if [[ "$tier" != pr ]]; then
  [[ -z "$(git status --porcelain --untracked-files=all)" ]] || fail 'release tier requires a clean tree (including untracked files)'
fi
if [[ "$only" == all || "$only" == gates ]]; then
  # Without an explicit URL the worker provisions a disposable PostgreSQL
  # container for this run and removes it afterwards; no manual step remains.
  if [[ -n "${SECONDBOX_TEST_DATABASE_URL:-}" ]]; then
    [[ "$SECONDBOX_TEST_DATABASE_URL" =~ ^postgres(ql)?:// ]] || fail 'invalid test PostgreSQL URL'
    export QUALIFY_PROVISION_TEST_DB=0
  else
    export QUALIFY_PROVISION_TEST_DB=1
    export QUALIFY_TEST_POSTGRES_IMAGE="${QUALIFY_TEST_POSTGRES_IMAGE:-docker.io/library/postgres@sha256:882236b897e39051d2368c5ccc6cda944904723506b2dfc97f2a8f5bc9afa382}"
    [[ "$QUALIFY_TEST_POSTGRES_IMAGE" =~ ^[a-z0-9./_-]+@sha256:[a-f0-9]{64}$ ]] || fail 'QUALIFY_TEST_POSTGRES_IMAGE must be a digest-pinned image reference'
  fi
  for tool in npm jq docker rg golangci-lint; do command -v "$tool" >/dev/null || fail "missing tool: $tool"; done
  [[ -x node_modules/.bin/tsc ]] || fail 'run npm ci --ignore-scripts first'
fi
if [[ "$only" == all || "$only" == firecracker ]]; then
  for key in SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY SECONDBOX_RUNNER_WORKSPACE_ROOT; do
    [[ "${!key:-}" == /* && -e "${!key}" ]] || fail "$key must be an existing absolute path"
  done
  [[ -d "$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR" && -d "$SECONDBOX_RUNNER_WORKSPACE_ROOT" && -f "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY" ]] || fail 'invalid Firecracker input paths'
  [[ "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256:-}" =~ ^[a-f0-9]{64}$ ]] || fail 'invalid artifact key fingerprint'
  for tool in openssl sha256sum docker; do command -v "$tool" >/dev/null || fail "missing tool: $tool"; done
  actual="$(openssl pkey -pubin -in "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY" -outform DER | sha256sum | cut -d' ' -f1)"
  [[ "$actual" == "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256" ]] || fail 'artifact public key fingerprint mismatch'
  for key in SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST; do
    [[ "${!key:-}" =~ ^sha256:[a-f0-9]{64}$ ]] || fail "$key must be a sha256 digest"
  done
  [[ "${SECONDBOX_REQUIRE_QUALIFIED_SCENARIO:-}" == 1 ]] || fail 'SECONDBOX_REQUIRE_QUALIFIED_SCENARIO must be 1'
  [[ -c /dev/kvm && -c /dev/net/tun ]] || fail 'Firecracker requires /dev/kvm and /dev/net/tun'
  docker info >/dev/null || fail 'Docker unavailable'
fi
if [[ "$only" == gvisor || ( "$only" == all && "$tier" != pr ) ]]; then
  if [[ "$tier" != nightly ]]; then
    ((QUALIFY_FIRECRACKER_SHARDS <= 7)) || fail 'local gVisor sharding supports at most seven network profile pairs'
  fi
  scripts/qualify-gvisor.sh --host --preflight
  if [[ "$tier" == nightly ]]; then scripts/qualify-gvisor.sh --preflight; fi
fi
if $preflight; then exit 0; fi
source_commit="$(git rev-parse HEAD)"
run="$(date -u +%Y%m%dT%H%M%S)-$$"
directory="$repo_root/.tmp/qualify/$run"
mkdir -p "$directory"
{ export -p; declare -p tier only source_commit caller_umask; } >"$directory/environment"
echo "Qualification run: $run ($tier, $only, $source_commit)"
if command -v systemd-run >/dev/null && systemctl --user show-environment >/dev/null 2>&1; then
  systemd-run --user --unit="secondbox-suite-qualify-$run" --collect \
    --property="WorkingDirectory=$repo_root" --property="StandardOutput=file:$directory/supervisor.log" \
    --property=StandardError=inherit --setenv="PATH=$PATH" --setenv="HOME=$HOME" \
    /bin/bash "$repo_root/scripts/qualify.sh" --worker "$directory"
else
  setsid /bin/bash "$repo_root/scripts/qualify.sh" --worker "$directory" >"$directory/supervisor.log" 2>&1 < /dev/null &
fi
for ((attempt=0; attempt<50; attempt++)); do [[ -f "$directory/pid" ]] && break; sleep 0.1; done
wait_run "$run"
