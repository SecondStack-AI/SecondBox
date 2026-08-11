#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
coordinate='https://github.com/SecondStack-AI/SecondBox/releases/latest/download/install.sh'

rg -qF "$coordinate" "$repo_root/README.md"
rg -qF "$coordinate" "$repo_root/docs/operations/guided-single-host-install.md"
rg -qF 'install --check' "$repo_root/README.md" "$repo_root/docs/operations/guided-single-host-install.md"
rg -qF 'sh -s -- update "$operation"' "$repo_root/docs/operations/guided-single-host-install.md"
rg -qF 'sh -s -- update /absolute/path/to/secondbox-install-operation' "$repo_root/README.md"
rg -qF -- '--resume DIRECTORY, --recover-compose-network DIRECTORY, or --support DIRECTORY --output ARCHIVE' "$repo_root/cmd/secondbox-deploy/help.go"
rg -qF 'releases/download/v0.4.7/install.sh' "$repo_root/docs/operations/guided-single-host-install.md"
rg -qF 'sh -s -- --recover-compose-network "$operation"' "$repo_root/docs/operations/guided-single-host-install.md"
rg -qF 'use --purge for deletion' "$repo_root/cmd/secondbox-deploy/help.go"
rg -qF 'update a completed guided deployment; use --check or --resume' "$repo_root/cmd/secondbox-deploy/help.go"
rg -qF 'ref("install.sh")' "$repo_root/cmd/secondbox-release-tool/main.go"
rg -qF 'install.sh$' "$repo_root/scripts/test-release-stage.sh"
if rg -q 'releases/download/v0\.1\.4' "$repo_root/README.md"; then
  echo 'README resurrected stale v0.1.4 installation instructions' >&2
  exit 1
fi

temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT
"$repo_root/scripts/generate-install-bootstrap.sh" 1.2.3 "$(printf fixture | sha256sum | awk '{print $1}')" "$temporary/install.sh"
sh -n "$temporary/install.sh"
rg -qF "version='1.2.3'" "$temporary/install.sh"
rg -qF 'secondbox-deploy_${version}_linux_amd64' "$temporary/install.sh"
rg -qF '"$binary" install </dev/tty &' "$temporary/install.sh"
rg -qF '"$binary" update "$@" </dev/tty &' "$temporary/install.sh"
rg -qF "trap 'terminate TERM 143' TERM" "$temporary/install.sh"
! rg -qF 'exec "$binary"' "$temporary/install.sh"
! rg -q '(^|[[:space:]])sudo([[:space:]]|$)|\.profile|\.bashrc|systemctl|mkfs' "$temporary/install.sh"

probe="$temporary/signal-probe"
mkdir -p "$probe/bin"
cat >"$probe/child" <<'EOF'
#!/bin/sh
printf '%s\n' started >"$BOOTSTRAP_CHILD_STARTED"
trap 'exit 143' TERM
while :; do sleep 1; done
EOF
cat >"$probe/bin/curl" <<'EOF'
#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = --output ]; then
    cp "$BOOTSTRAP_FIXTURE_BINARY" "$2"
    exit
  fi
  shift
done
exit 1
EOF
cat >"$probe/bin/mktemp" <<'EOF'
#!/bin/sh
mkdir "$BOOTSTRAP_DOWNLOAD_DIRECTORY"
printf '%s\n' "$BOOTSTRAP_DOWNLOAD_DIRECTORY"
EOF
chmod 0755 "$probe/child" "$probe/bin/curl" "$probe/bin/mktemp"
signal_bootstrap="$temporary/signal-install.sh"
"$repo_root/scripts/generate-install-bootstrap.sh" 1.2.3 "$(sha256sum "$probe/child" | awk '{print $1}')" "$signal_bootstrap"
BOOTSTRAP_FIXTURE_BINARY="$probe/child" \
BOOTSTRAP_CHILD_STARTED="$probe/started" \
BOOTSTRAP_DOWNLOAD_DIRECTORY="$probe/download" \
PATH="$probe/bin:$PATH" \
  sh "$signal_bootstrap" --check &
wrapper_pid=$!
for _ in $(seq 1 100); do
  [[ -e "$probe/started" ]] && break
  sleep 0.01
done
[[ -e "$probe/started" ]] || { echo 'install bootstrap child did not start' >&2; exit 1; }
kill -TERM "$wrapper_pid"
set +e
wait "$wrapper_pid"
wrapper_status=$?
set -e
[[ "$wrapper_status" -eq 143 ]] || { echo "install bootstrap TERM status was $wrapper_status, want 143" >&2; exit 1; }
[[ ! -e "$probe/download" ]] || { echo 'install bootstrap did not clean its download after child termination' >&2; exit 1; }

cat >"$probe/dispatch-child" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >"$BOOTSTRAP_DISPATCH_ARGUMENTS"
EOF
chmod 0755 "$probe/dispatch-child"
dispatch_bootstrap="$temporary/dispatch-install.sh"
"$repo_root/scripts/generate-install-bootstrap.sh" 1.2.3 "$(sha256sum "$probe/dispatch-child" | awk '{print $1}')" "$dispatch_bootstrap"
BOOTSTRAP_FIXTURE_BINARY="$probe/dispatch-child" \
BOOTSTRAP_DISPATCH_ARGUMENTS="$probe/dispatch-arguments" \
BOOTSTRAP_DOWNLOAD_DIRECTORY="$probe/dispatch-download" \
PATH="$probe/bin:$PATH" \
  sh "$dispatch_bootstrap" update --check /srv/secondbox-operation
[[ "$(cat "$probe/dispatch-arguments")" == 'update --check /srv/secondbox-operation' ]] || {
  echo 'install bootstrap did not dispatch update to the verified target binary' >&2
  exit 1
}
BOOTSTRAP_FIXTURE_BINARY="$probe/dispatch-child" \
BOOTSTRAP_DISPATCH_ARGUMENTS="$probe/dispatch-arguments" \
BOOTSTRAP_DOWNLOAD_DIRECTORY="$probe/dispatch-download" \
PATH="$probe/bin:$PATH" \
  sh "$dispatch_bootstrap" update --resume /srv/secondbox-operation </dev/null
[[ "$(cat "$probe/dispatch-arguments")" == 'update --resume /srv/secondbox-operation' ]] || {
  echo 'install bootstrap did not dispatch noninteractive update recovery' >&2
  exit 1
}
