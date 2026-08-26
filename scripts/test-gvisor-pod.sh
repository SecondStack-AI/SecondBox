#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# Runs the gVisor backend qualification suite inside a privileged pod on a
# Kubernetes node without KVM, then proves the pod-specific containment
# properties: sandbox cgroups nest inside the pod's slice with per-sandbox
# limits, and no Workspace mount leaks into the host mount table. Run it on
# the node itself (kubectl and the host cgroup/mount tables must be local).

fail() {
  echo "SecondBox gVisor pod qualification: $*" >&2
  exit 1
}

[[ "$(uname -s)" == "Linux" && "$(uname -m)" == "x86_64" ]] || fail "requires Linux x86_64"
[[ ! -e /dev/kvm ]] || fail "qualifies nodes without /dev/kvm"
[[ "$(id -u)" == "0" ]] || fail "requires root for kubectl and host-table checks"

: "${SECONDBOX_GVISOR_POD_KUBECTL:=k3s kubectl}"
: "${SECONDBOX_GVISOR_POD_IMAGE:?set SECONDBOX_GVISOR_POD_IMAGE to the imported qualification image reference}"
: "${SECONDBOX_GVISOR_POD_BUILD:?set SECONDBOX_GVISOR_POD_BUILD to the node-local gVisor build directory}"
: "${SECONDBOX_GVISOR_POD_TEST_BINARY:?set SECONDBOX_GVISOR_POD_TEST_BINARY to the node-local compiled internal/gvisor test binary}"
: "${SECONDBOX_GVISOR_POD_REFLINK:?set SECONDBOX_GVISOR_POD_REFLINK to a node-local reflink-capable directory}"

for path in "$SECONDBOX_GVISOR_POD_BUILD/bin/runsc" "$SECONDBOX_GVISOR_POD_BUILD/bin/secondbox-guest-agent" "$SECONDBOX_GVISOR_POD_TEST_BINARY"; do
  [[ -f "$path" && -x "$path" ]] || fail "missing executable input: $path"
done
[[ -d "$SECONDBOX_GVISOR_POD_BUILD/rootfs" ]] || fail "missing flat root: $SECONDBOX_GVISOR_POD_BUILD/rootfs"
[[ -d "$SECONDBOX_GVISOR_POD_REFLINK" ]] || fail "missing reflink directory: $SECONDBOX_GVISOR_POD_REFLINK"

kubectl() {
  # shellcheck disable=SC2086
  $SECONDBOX_GVISOR_POD_KUBECTL "$@"
}

pod_name="secondbox-gvisor-qualification"
cleanup() {
  status="$?"
  trap - EXIT
  kubectl delete pod "$pod_name" --ignore-not-found --wait=true >/dev/null 2>&1 || status=1
  exit "$status"
}
trap cleanup EXIT

kubectl delete pod "$pod_name" --ignore-not-found --wait=true >/dev/null

kubectl apply -f - <<POD
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  labels:
    app.kubernetes.io/name: $pod_name
spec:
  restartPolicy: Never
  containers:
    - name: qualification
      image: $SECONDBOX_GVISOR_POD_IMAGE
      imagePullPolicy: Never
      command: ["/bin/sh", "-c", "sleep infinity"]
      securityContext:
        privileged: true
      resources:
        requests:
          cpu: "4"
          memory: 4Gi
        limits:
          cpu: "4"
          memory: 4Gi
      volumeMounts:
        - name: build
          mountPath: /opt/secondbox-gvisor-qualification
          readOnly: true
        - name: test-binary
          mountPath: /opt/secondbox-gvisor-qualification-test
          readOnly: true
        - name: reflink
          mountPath: /var/lib/secondbox-reflink
  volumes:
    - name: build
      hostPath:
        path: $SECONDBOX_GVISOR_POD_BUILD
        type: Directory
    - name: test-binary
      hostPath:
        path: $SECONDBOX_GVISOR_POD_TEST_BINARY
        type: File
    - name: reflink
      hostPath:
        path: $SECONDBOX_GVISOR_POD_REFLINK
        type: Directory
POD

kubectl wait --for=condition=Ready "pod/$pod_name" --timeout=180s >/dev/null

# The suite runs in the background of the pod while the host side samples the
# node's cgroup and mount tables mid-flight.
kubectl exec "$pod_name" -- sh -c '
  SECONDBOX_GVISOR_QUALIFICATION=1 \
  SECONDBOX_GVISOR_BUILD=/opt/secondbox-gvisor-qualification \
  SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM=/var/lib/secondbox-reflink \
  /opt/secondbox-gvisor-qualification-test -test.run "TestQualified|TestAttachment" \
    > /tmp/pod-qualification.log 2>&1
  echo "exit=$?" >> /tmp/pod-qualification.log
' &
suite_pid="$!"

nested_cgroup=""
nested_limits=""
for _ in $(seq 1 120); do
  candidate="$(find /sys/fs/cgroup/kubepods.slice -maxdepth 4 -type d -name 'secondbox-gvisor-p*' 2>/dev/null | head -1)"
  if [[ -n "$candidate" ]]; then
    instance="$(find "$candidate" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -1)"
    if [[ -n "$instance" ]]; then
      nested_cgroup="$instance"
      nested_limits="cpu.max=$(cat "$instance/cpu.max" 2>/dev/null) memory.max=$(cat "$instance/memory.max" 2>/dev/null)"
      # The suite launches one-vCPU, 256 MiB Instances; the nested limits must
      # state exactly those values or the per-sandbox enforcement regressed.
      [[ "$(cat "$instance/cpu.max" 2>/dev/null)" == "100000 100000" ]] ||
        fail "nested sandbox cpu.max is not the one-vCPU quota: $(cat "$instance/cpu.max" 2>/dev/null)"
      [[ "$(cat "$instance/memory.max" 2>/dev/null)" == "268435456" ]] ||
        fail "nested sandbox memory.max is not the 256 MiB limit: $(cat "$instance/memory.max" 2>/dev/null)"
      # While the sandbox is live, its Workspace mount must exist only inside
      # the supervisor's private mount namespace. The backend mounts each
      # loop-attached image at <runtime>/<instance>/mnt, so the host table
      # must show no target under the qualification runtime prefix and no
      # ext4 mount whose source is a loop device backed by the reflink store.
      if findmnt -rn -o TARGET | grep -q '^/run/sbxgv-\|/secondbox-gvisor/'; then
        fail "a Workspace runtime mountpoint is visible in the host mount table"
      fi
      while read -r source target; do
        backing="$(losetup -nO BACK-FILE "$source" 2>/dev/null || cat "/sys/block/$(basename "$source")/loop/backing_file" 2>/dev/null)"
        case "$backing" in
          "$SECONDBOX_GVISOR_POD_REFLINK"/*)
            fail "a loop-backed Workspace mount is visible in the host mount table: $source -> $target" ;;
        esac
      done < <(findmnt -rn -t ext4 -o SOURCE,TARGET | grep '^/dev/loop' || true)
      break
    fi
  fi
  sleep 1
done

wait "$suite_pid" || fail "suite exec transport failed"
suite_log="$(kubectl exec "$pod_name" -- cat /tmp/pod-qualification.log)"
printf '%s\n' "$suite_log" | tail -20
printf '%s\n' "$suite_log" | grep -q '^exit=0$' || fail "qualification suite failed inside the pod"

[[ -n "$nested_cgroup" ]] || fail "no sandbox cgroup observed inside the pod slice during the suite"
echo "nested sandbox cgroup: $nested_cgroup"
echo "nested sandbox limits: $nested_limits"

! findmnt -rn -o TARGET | grep -q 'secondbox-gvisor' ||
  fail "a Workspace mount leaked into the host mount table"

leftover="$(find /sys/fs/cgroup/kubepods.slice -maxdepth 5 -type d -name 'secondbox-gvisor-p*' 2>/dev/null | head -1)"
if [[ -n "$leftover" ]] && find "$leftover" -mindepth 1 -maxdepth 1 -type d | grep -q .; then
  fail "sandbox cgroups were not cleaned up: $leftover"
fi

echo "SecondBox gVisor pod qualification passed"
