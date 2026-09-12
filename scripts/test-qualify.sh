#!/usr/bin/env bash
set -Eeuo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf -- "$temporary"' EXIT
mkdir -p "$temporary/repo/scripts" "$temporary/home/.config/secondbox" "$temporary/bin" "$temporary/repo/node_modules/.bin"
cp "$repo_root/scripts/qualify.sh" "$temporary/repo/scripts/"
cat >"$temporary/bin/just" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[[ "$(umask)" == "$QUALIFY_TEST_UMASK" ]] || exit 91
# Keep one gate running long enough to prove other gate processes launch.
if [[ "$1" == test ]]; then
  for ((i=0; i<100; i++)); do
    [[ -e "$QUALIFY_TEST_MARKER" ]] && exit 0
    sleep 0.05
  done
  exit 90
fi
if [[ "$1" == test-contract ]]; then touch "$QUALIFY_TEST_MARKER"; exit 17; fi
exit 0
STUB
cat >"$temporary/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
chmod +x "$temporary/bin/just" "$temporary/bin/systemctl"
touch "$temporary/repo/node_modules/.bin/tsc"
chmod +x "$temporary/repo/node_modules/.bin/tsc"
export HOME="$temporary/home" PATH="$temporary/bin:$PATH"
export SECONDBOX_TEST_DATABASE_URL=postgresql://test.invalid/disposable
export QUALIFY_TEST_MARKER="$temporary/concurrent"
export QUALIFY_TEST_UMASK="$(umask)"
printf 'QUALIFY_FIRECRACKER_SHARDS=1\n' >"$HOME/.config/secondbox/qualify.env"
cd "$temporary/repo"
git init -q
git -c user.name=Qualification -c user.email=qualification@example.invalid add scripts
git -c user.name=Qualification -c user.email=qualification@example.invalid commit -qm fixture
expect_failure() {
  local expected="$1"; shift
  if "$@" >"$temporary/output" 2>&1; then echo "unexpected success: $*" >&2; exit 1; fi
  rg -q -- "$expected" "$temporary/output"
}
expect_failure 'tier must be' scripts/qualify.sh --tier invalid
expect_failure 'missing value' scripts/qualify.sh --only
expect_failure 'invalid run ID' scripts/qualify.sh --wait ../escape
expect_failure 'requires a clean tree' scripts/qualify.sh --tier release --only gates
printf 'QUALIFY_FIRECRACKER_SHARDS=0\n' >"$HOME/.config/secondbox/qualify.env"
expect_failure 'must be 1..64' scripts/qualify.sh --only gates
printf 'QUALIFY_FIRECRACKER_SHARDS=1\n' >"$HOME/.config/secondbox/qualify.env"
expect_failure 'FAIL \(17\)' scripts/qualify.sh --only gates
run="$(basename "$(find .tmp/qualify -mindepth 1 -maxdepth 1 -type d)")"
[[ "$(cat ".tmp/qualify/$run/result")" == 1 ]]
[[ "$(wc -l <".tmp/qualify/$run/timing.md")" == 13 ]]
rg -q '\| test \| PASS' ".tmp/qualify/$run/timing.md"
expect_failure 'FAIL \(17\)' scripts/qualify.sh --wait "$run"
echo 'SecondBox qualification orchestration tests passed'
