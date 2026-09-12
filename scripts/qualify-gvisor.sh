#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
fail() { echo "SecondBox gVisor qualification: $*" >&2; exit 1; }

# This entry point runs in the dedicated VM, under a root systemd service.
if [[ "${1:-}" == --guest ]]; then
  source "$2"
  export HOME=/root GOPATH=/root/go KUBECACHEDIR=/root/.kube/cache
  export PATH=/usr/local/bin/go/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
  export SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1
  export SECONDBOX_GVISOR_LINUX_BUILD="$QUALIFY_GVISOR_BUILD_ROOT"
  export SECONDBOX_RUNNER_WORKSPACE_ROOT="$QUALIFY_GVISOR_REFLINK_MOUNT/qualify-$run"
  cd "$QUALIFY_GVISOR_REPO"
  status=0
  finish() {
    local code=$?
    ((code == 0)) || status=$code
    chown -R "$QUALIFY_GVISOR_SSH_USER:$QUALIFY_GVISOR_SSH_USER" .git || status=1
    echo "$status" >"$remote/result"
  }
  trap finish EXIT
  [[ ! -e /dev/kvm ]] || fail 'VM exposes /dev/kvm'
  export GOTOOLCHAIN="go$(awk '$1 == "go" {print $2; exit}' go.mod)"
  agent="$(mktemp)"
  (cd runner && CGO_ENABLED=0 go build -trimpath -o "$agent" ./cmd/secondbox-guest-agent)
  install -m 0755 "$agent" "$QUALIFY_GVISOR_BUILD_ROOT/bin/secondbox-guest-agent"
  rm -- "$agent"
  mountpoint -q "$QUALIFY_GVISOR_REFLINK_MOUNT" || mount -o loop "$QUALIFY_GVISOR_REFLINK_IMAGE" "$QUALIFY_GVISOR_REFLINK_MOUNT"
  mkdir -p "$SECONDBOX_RUNNER_WORKSPACE_ROOT"
  for suite in gvisor gvisor-pod; do
    start=$SECONDS code=0
    name="$suite"; [[ "$suite" != gvisor ]] || name=gvisor-host
    scripts/test-scenario-$suite.sh >"$remote/$name.log" 2>&1 || code=$?
    printf '%s\t%s\t%s\n' "$name" "$code" "$((SECONDS-start))" >"$remote/$name.status"
    ((code == 0)) || status=1
  done
  exit "$status"
fi

for key in QUALIFY_GVISOR_VM_DIR QUALIFY_GVISOR_REPO QUALIFY_GVISOR_BUILD_ROOT QUALIFY_GVISOR_REFLINK_IMAGE QUALIFY_GVISOR_REFLINK_MOUNT; do
  # Restrict remote paths to shell-safe absolute paths; no eval of operator values.
  [[ "${!key:-}" =~ ^/[a-zA-Z0-9_./-]+$ && "${!key}" != / && "${!key}" != *'/../'* ]] || fail "$key must be an absolute path without shell metacharacters"
done
[[ "${QUALIFY_GVISOR_SSH_PORT:-}" =~ ^[1-9][0-9]*$ ]] && ((QUALIFY_GVISOR_SSH_PORT <= 65535)) || fail 'invalid VM SSH port'
[[ "${QUALIFY_GVISOR_SSH_USER:-}" =~ ^[a-z_][a-z0-9_-]*$ ]] || fail 'invalid VM SSH user'
for file in disk.qcow2 seed.img id_ed25519 known_hosts; do
  [[ -f "$QUALIFY_GVISOR_VM_DIR/$file" ]] || fail "missing VM input: $file"
done
for tool in ssh scp qemu-system-x86_64 jq; do command -v "$tool" >/dev/null || fail "missing tool: $tool"; done
ssh_options=(-i "$QUALIFY_GVISOR_VM_DIR/id_ed25519" -o "UserKnownHostsFile=$QUALIFY_GVISOR_VM_DIR/known_hosts" -o StrictHostKeyChecking=yes -o BatchMode=yes -o ConnectTimeout=5 -o ServerAliveInterval=15 -o ServerAliveCountMax=4)
ssh_vm() { ssh "${ssh_options[@]}" -p "$QUALIFY_GVISOR_SSH_PORT" "$QUALIFY_GVISOR_SSH_USER@127.0.0.1" "$@"; }
if [[ "${1:-}" == --preflight ]]; then
  # If online, validate remote prerequisites before any local stages start.
  if (echo >/dev/tcp/127.0.0.1/"$QUALIFY_GVISOR_SSH_PORT") 2>/dev/null; then
    ssh_vm "test ! -e /dev/kvm && sudo -n true && test -d $QUALIFY_GVISOR_REPO/.git && test -x $QUALIFY_GVISOR_BUILD_ROOT/bin/runsc && test -d $QUALIFY_GVISOR_BUILD_ROOT/rootfs && test -f $QUALIFY_GVISOR_REFLINK_IMAGE && test -d $QUALIFY_GVISOR_REFLINK_MOUNT && sudo -n docker info >/dev/null && sudo -n k3s kubectl get nodes >/dev/null" || fail 'VM prerequisites failed'
  fi
  exit
fi

directory="$1" source_commit="$2"
run="$(basename "$directory")"
remote="/tmp/secondbox-suite-qualify-$run"
exec 8>"$QUALIFY_GVISOR_VM_DIR/qualify.lock"
flock -n 8 || fail 'another qualification owns this VM'
if ! (echo >/dev/tcp/127.0.0.1/"$QUALIFY_GVISOR_SSH_PORT") 2>/dev/null; then
  if [[ -f "$QUALIFY_GVISOR_VM_DIR/qemu.pid" ]] && kill -0 "$(cat "$QUALIFY_GVISOR_VM_DIR/qemu.pid")" 2>/dev/null; then
    echo 'QEMU is already running; waiting for SSH'
  else
    (cd "$QUALIFY_GVISOR_VM_DIR" && qemu-system-x86_64 \
      -machine q35,accel=kvm -cpu host,-vmx,-svm -smp 8 -m 8G \
      -drive if=virtio,file=disk.qcow2 -drive if=virtio,format=raw,file=seed.img \
      -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$QUALIFY_GVISOR_SSH_PORT-:22" \
      -display none -serial file:console.log -pidfile qemu.pid -daemonize)
  fi
fi
ready=false
for ((attempt=0; attempt<60; attempt++)); do
  if ssh_vm true; then ready=true; break; fi
  sleep 2
done
$ready || fail 'VM SSH did not become ready'
# Ship a self-contained commit without creating or moving any branch or tag.
git bundle create "$directory/source.bundle" HEAD
ssh_vm "mkdir -m 700 $remote"
scp "${ssh_options[@]}" -P "$QUALIFY_GVISOR_SSH_PORT" "$directory/source.bundle" "$QUALIFY_GVISOR_SSH_USER@127.0.0.1:$remote/source.bundle"
{
  declare -p QUALIFY_GVISOR_REPO QUALIFY_GVISOR_BUILD_ROOT QUALIFY_GVISOR_REFLINK_IMAGE QUALIFY_GVISOR_REFLINK_MOUNT QUALIFY_GVISOR_SSH_USER run remote
} >"$directory/guest.env"
scp "${ssh_options[@]}" -P "$QUALIFY_GVISOR_SSH_PORT" "$directory/guest.env" "$QUALIFY_GVISOR_SSH_USER@127.0.0.1:$remote/guest.env"
# The remote lock covers checkout and the complete chain, including other hosts.
ssh_vm bash -s -- "$QUALIFY_GVISOR_REPO" "$source_commit" "$remote" "$run" "$QUALIFY_GVISOR_SSH_USER" <<'REMOTE'
set -euo pipefail
repo="$1" commit="$2" remote="$3" run="$4" user="$5"
exec 9>/tmp/secondbox-suite-qualify.lock
flock -n 9 || { echo 'SecondBox gVisor VM is already qualifying'; exit 1; }
sudo chown -R "$user:$user" "$repo"
cd "$repo"
[[ -z "$(git status --porcelain --untracked-files=all)" ]] || { echo 'SecondBox gVisor VM checkout is dirty'; exit 1; }
git fetch "$remote/source.bundle" HEAD
git checkout --detach "$commit"
[[ "$(git rev-parse HEAD)" == "$commit" ]]
sudo systemd-run --unit="secondbox-suite-qualify-$run" --collect \
  --property="WorkingDirectory=$repo" --property="StandardOutput=file:$remote/chain.log" --property=StandardError=inherit \
  /bin/bash "$repo/scripts/qualify-gvisor.sh" --guest "$remote/guest.env"
while [[ ! -f "$remote/result" ]]; do
  state="$(systemctl is-active "secondbox-suite-qualify-$run" || true)"
  if [[ "$state" != active && "$state" != activating ]]; then
    [[ -f "$remote/result" ]] || { sudo cat "$remote/chain.log"; exit 1; }
  fi
  sleep 2
done
sudo chmod -R a+rX "$remote"
REMOTE
scp "${ssh_options[@]}" -P "$QUALIFY_GVISOR_SSH_PORT" "$QUALIFY_GVISOR_SSH_USER@127.0.0.1:$remote/*.log" "$QUALIFY_GVISOR_SSH_USER@127.0.0.1:$remote/*.status" "$directory/"
# Keep the guest result separate from the supervisor result.
ssh_vm "cat $remote/result" >"$directory/gvisor-result"
[[ "$(cat "$directory/gvisor-result")" == 0 ]] || fail 'VM chain failed; see gvisor and gvisor-pod logs'
for suite in gvisor gvisor-pod; do
  evidence="$suite-linux-scenario-qualification-evidence.json"
  ssh_vm "sudo cat $QUALIFY_GVISOR_REPO/.tmp/$evidence" >"$directory/$evidence"
  jq -e --arg commit "$source_commit" '.sourceCommit == $commit and .repositoryDirty == false' "$directory/$evidence" >/dev/null || fail "invalid $suite evidence identity"
  cp "$directory/$evidence" ".tmp/$evidence"
done
