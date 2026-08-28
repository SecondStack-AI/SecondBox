#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# Service control for the gVisor pod scenario placement: the suite drives the
# runner with Compose-shaped verbs, and this script translates them onto a
# privileged Kubernetes pod per runner while postgres and the control plane
# stay in Compose. It runs on the node itself with the scenario driver's
# environment exported.

fail() {
  echo "SecondBox gVisor pod scenario service control failed: $*" >&2
  exit 1
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

for name in \
  SECONDBOX_SCENARIO_COMPOSE_PROJECT \
  SECONDBOX_SCENARIO_COMPOSE_FILE \
  SECONDBOX_SCENARIO_RUNNER_IMAGE \
  SECONDBOX_SCENARIO_POD_CONTROL_PLANE_HOST \
  SECONDBOX_SCENARIO_GVISOR_BUILD \
  SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION \
  SECONDBOX_SCENARIO_IDENTITY_DIR \
  SECONDBOX_SCENARIO_STATE_DIR \
  SECONDBOX_SCENARIO_WORKSPACE_DIR \
  SECONDBOX_SCENARIO_RELOCATION_IDENTITY_DIR \
  SECONDBOX_SCENARIO_RELOCATION_STATE_DIR \
  SECONDBOX_SCENARIO_RELOCATION_WORKSPACE_DIR; do
  [[ -n "${!name:-}" ]] || fail "$name is required"
done

kubectl_command=(${SECONDBOX_SCENARIO_POD_KUBECTL:-k3s kubectl})
# kubectl writes its discovery cache under $HOME; pin it outside the
# repository so a qualification run cannot dirty the source tree.
export KUBECACHEDIR="${TMPDIR:-/tmp}/secondbox-scenario-kubecache"
kubectl() { "${kubectl_command[@]}" --cache-dir "$KUBECACHEDIR" "$@"; }

if docker compose version >/dev/null 2>&1; then
  compose=(docker compose)
elif command -v docker-compose >/dev/null 2>&1 && docker-compose version >/dev/null 2>&1; then
  compose=(docker-compose)
else
  fail "Docker Compose v2 is required"
fi
compose+=(
  --project-name "$SECONDBOX_SCENARIO_COMPOSE_PROJECT"
  --file "$SECONDBOX_SCENARIO_COMPOSE_FILE"
)
if [[ -n "${SECONDBOX_SCENARIO_COMPOSE_OVERRIDE_FILE:-}" ]]; then
  compose+=(--file "$SECONDBOX_SCENARIO_COMPOSE_OVERRIDE_FILE")
fi

service_paths() {
  case "$1" in
    secondbox-runner)
      service_runner_id="$SECONDBOX_RUNNER_ID"
      service_identity="$SECONDBOX_SCENARIO_IDENTITY_DIR"
      service_state="$SECONDBOX_SCENARIO_STATE_DIR"
      service_workspace="$SECONDBOX_SCENARIO_WORKSPACE_DIR"
      service_port="$SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT"
      service_network_profile="0"
      ;;
    secondbox-runner-relocation)
      service_runner_id="$SECONDBOX_SCENARIO_RELOCATION_RUNNER_ID"
      service_identity="$SECONDBOX_SCENARIO_RELOCATION_IDENTITY_DIR"
      service_state="$SECONDBOX_SCENARIO_RELOCATION_STATE_DIR"
      service_workspace="$SECONDBOX_SCENARIO_RELOCATION_WORKSPACE_DIR"
      service_port="$SECONDBOX_SCENARIO_RELOCATION_RUNNER_DATA_PLANE_PORT"
      # Both runner pods share the node's loop devices and, through hostPort,
      # its port space; the network profile keeps their DNS proxies, slot
      # spaces, and link names apart.
      service_network_profile="1"
      ;;
    *)
      fail "unknown runner service: $1"
      ;;
  esac
}

import_runner_image() {
  docker save "$SECONDBOX_SCENARIO_RUNNER_IMAGE" |
    ${SECONDBOX_SCENARIO_POD_CTR:-k3s ctr} images import - >/dev/null ||
    fail "runner image import into containerd failed"
}

emit_env() {
  local entry
  for entry in "$@"; do
    printf '        - name: %s\n          value: "%s"\n' "${entry%%=*}" "${entry#*=}"
  done
}

runner_start() {
  local service="$1"
  [[ -n "$service" ]] || fail "a runner service is required"
  service_paths "$service"
  import_runner_image
  kubectl apply -f - <<POD
apiVersion: v1
kind: Pod
metadata:
  name: $service
  labels:
    app.kubernetes.io/name: $service
    org.secondbox.runner.qualification: scenario-suite
spec:
  restartPolicy: Always
  terminationGracePeriodSeconds: 45
  containers:
    - name: runner
      image: docker.io/library/$SECONDBOX_SCENARIO_RUNNER_IMAGE:latest
      imagePullPolicy: Never
      command: ["/usr/local/bin/secondbox-scenario-runner-entrypoint"]
      securityContext:
        privileged: true
      resources:
        # Sized so both scenario runner pods - the primary and the
        # relocation target - co-schedule on the 8-vCPU / 8-GiB
        # qualification node beside the system pods; the scenario sandbox
        # budget (2 vCPU / 2 GiB) nests comfortably inside.
        requests:
          cpu: "3"
          memory: 3Gi
        limits:
          cpu: "3"
          memory: 3Gi
      ports:
        - containerPort: $service_port
          hostPort: $service_port
          protocol: TCP
      readinessProbe:
        exec:
          command: ["/usr/local/bin/secondbox-runner", "-healthcheck", "-healthcheck-timeout", "5s"]
        initialDelaySeconds: 3
        periodSeconds: 3
        timeoutSeconds: 6
        failureThreshold: 20
      env:
$(emit_env \
  "SECONDBOX_RUNNER_ID=$service_runner_id" \
  "SECONDBOX_COMPUTE_BACKEND=$SECONDBOX_COMPUTE_BACKEND" \
  "SECONDBOX_RUNNER_POOL_ID=$SECONDBOX_RUNNER_POOL_ID" \
  "SECONDBOX_RUNNER_SOFTWARE_VERSION=$SECONDBOX_SCENARIO_SOURCE_COMMIT" \
  "SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS=$SECONDBOX_SCENARIO_POD_CONTROL_PLANE_HOST:$SECONDBOX_SCENARIO_RUNNER_PORT" \
  "SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME=localhost" \
  "SECONDBOX_RUNNER_CREDENTIAL=$SECONDBOX_SCENARIO_RUNNER_CREDENTIAL" \
  "SECONDBOX_RUNNER_CLIENT_CERTIFICATE=$SECONDBOX_SCENARIO_RUNNER_CLIENT_CERTIFICATE" \
  "SECONDBOX_RUNNER_CLIENT_KEY=/opt/secondbox-runner-identity/runner.key" \
  "SECONDBOX_RUNNER_CONTROL_PLANE_CA=/opt/secondbox-runner-identity/runner-ca.crt" \
  "SECONDBOX_RUNNER_LOG_PATH=/var/lib/secondbox-runner/log/runner.jsonl" \
  "SECONDBOX_RUNNER_LOG_DIR=/var/lib/secondbox-runner/log" \
  "SECONDBOX_RUNNER_WORKSPACE_ROOT=/w" \
  "SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT=$SECONDBOX_SCENARIO_STORAGE_RECOVERY_PERCENT" \
  "SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT=$SECONDBOX_SCENARIO_STORAGE_WARNING_PERCENT" \
  "SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT=$SECONDBOX_SCENARIO_STORAGE_DENY_PERCENT" \
  "SECONDBOX_RUNNER_SANDBOX_MAX_VCPUS=$SECONDBOX_SCENARIO_SANDBOX_MAX_VCPUS" \
  "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB=$SECONDBOX_SCENARIO_SANDBOX_MEMORY_MIB" \
  "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB=$SECONDBOX_SCENARIO_SANDBOX_DISK_MIB" \
  "SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB=$SECONDBOX_SCENARIO_MEMORY_BUDGET_MIB" \
  "SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX=$SECONDBOX_SCENARIO_MAX_CONCURRENT_PER_SANDBOX" \
  "SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL=$SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL" \
  "SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS=$SECONDBOX_SCENARIO_MAX_CONCURRENT_STARTS" \
  "SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES=$SECONDBOX_SCENARIO_MAX_CONCURRENT_WORKSPACE_CREATES" \
  "SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL=$SECONDBOX_SCENARIO_MAX_CONCURRENT_OPERATIONS_GLOBAL" \
  "SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES=$SECONDBOX_SCENARIO_FILE_TRANSFER_MAX_BYTES" \
  "SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL=$SECONDBOX_SCENARIO_RUNNER_GUEST_HEARTBEAT_INTERVAL" \
  "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS=256" \
  "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL=5m" \
  "SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES=$SECONDBOX_SCENARIO_BRIDGE_ADDRESS" \
  "SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS=$SECONDBOX_SCENARIO_GUEST_CIDR" \
  "SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS=agent-gateway.secondbox.internal=$SECONDBOX_SCENARIO_BRIDGE_ADDRESS" \
  "SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM=1.1.1.1:53" \
  "SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS=0.0.0.0:$service_port" \
  "SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS=127.0.0.1:$service_port" \
  "SECONDBOX_GVISOR_RUNSC_PATH=/opt/secondbox-gvisor/bin/runsc" \
  "SECONDBOX_GVISOR_AGENT_PATH=/opt/secondbox-gvisor/bin/secondbox-guest-agent" \
  "SECONDBOX_GVISOR_FLAT_ROOT_PATH=/opt/secondbox-gvisor/rootfs" \
  "SECONDBOX_GVISOR_MATERIALIZATION_PATH=/opt/secondbox-gvisor-materialization.json" \
  "SECONDBOX_GVISOR_MATERIALIZATION_DIGEST=$SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION_DIGEST" \
  "SECONDBOX_GVISOR_RUNTIME_DIR=/run/secondbox-gvisor" \
  "SECONDBOX_GVISOR_MAXIMUM_VCPUS=$SECONDBOX_SCENARIO_GVISOR_MAXIMUM_VCPUS" \
  "SECONDBOX_GVISOR_MAXIMUM_MEMORY_BYTES=$SECONDBOX_SCENARIO_GVISOR_MAXIMUM_MEMORY_BYTES" \
  "SECONDBOX_GVISOR_MAXIMUM_DISK_BYTES=$SECONDBOX_SCENARIO_GVISOR_MAXIMUM_DISK_BYTES" \
  "SECONDBOX_GVISOR_MAXIMUM_INSTANCES=$SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL" \
  "SECONDBOX_GVISOR_MAXIMUM_OPERATIONS=$SECONDBOX_SCENARIO_MAX_CONCURRENT_OPERATIONS_GLOBAL" \
  "SECONDBOX_GVISOR_WORKSPACE_TEMPLATE_CAPACITY_BYTES=$SECONDBOX_SCENARIO_GVISOR_WORKSPACE_TEMPLATE_CAPACITY_BYTES" \
  "SECONDBOX_GVISOR_NETWORK_PROFILE=$service_network_profile" \
)
      volumeMounts:
        - name: entrypoint
          mountPath: /usr/local/bin/secondbox-scenario-runner-entrypoint
          readOnly: true
        - name: identity
          mountPath: /opt/secondbox-runner-identity
          readOnly: true
        - name: gvisor-build
          mountPath: /opt/secondbox-gvisor
          readOnly: true
        - name: gvisor-materialization
          mountPath: /opt/secondbox-gvisor-materialization.json
          readOnly: true
        - name: state
          mountPath: /var/lib/secondbox-runner
        - name: workspace
          mountPath: /w
  volumes:
    - name: entrypoint
      hostPath:
        path: $repo_root/scripts/scenario-runner-entrypoint.sh
        type: File
    - name: identity
      hostPath:
        path: $service_identity
        type: Directory
    - name: gvisor-build
      hostPath:
        path: $SECONDBOX_SCENARIO_GVISOR_BUILD
        type: Directory
    - name: gvisor-materialization
      hostPath:
        path: $SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION
        type: File
    - name: state
      hostPath:
        path: $service_state
        type: Directory
    - name: workspace
      hostPath:
        path: $service_workspace
        type: Directory
POD
  local timeout="${wait_timeout:-300}"
  kubectl wait --for=condition=Ready "pod/$service" --timeout="${timeout}s" >/dev/null ||
    fail "runner pod $service did not become ready"
}

runner_stop() {
  local service="$1" grace="${2:-45}"
  kubectl delete pod "$service" --ignore-not-found --wait=true \
    --grace-period="$grace" >/dev/null
}

runner_logs() {
  local service="$1" tail_lines="${2:-}"
  local -a arguments=(logs "pod/$service")
  if [[ -n "$tail_lines" ]]; then
    arguments+=("--tail=$tail_lines")
  fi
  kubectl "${arguments[@]}" 2>/dev/null || true
}

arguments=("$@")
strip_profile=()
index=0
while (( index < ${#arguments[@]} )); do
  if [[ "${arguments[$index]}" == "--profile" ]]; then
    index=$(( index + 2 ))
    continue
  fi
  strip_profile+=("${arguments[$index]}")
  index=$(( index + 1 ))
done
arguments=("${strip_profile[@]}")
(( ${#arguments[@]} > 0 )) || fail "a command is required"

case "${arguments[0]}" in
  helper-rss-kib)
    (( ${#arguments[@]} == 2 )) || fail "helper-rss-kib requires one PID"
    # Sum peak RSS across the compute process tree: the mount supervisor
    # parents the runsc sentry and gofer, whose memory is the measurement.
    kubectl exec secondbox-runner -- sh -c '
peak=0
walk() {
  v=$(awk '"'"'$1=="VmHWM:"{print $2}'"'"' "/proc/$1/status" 2>/dev/null)
  [ -n "$v" ] && peak=$((peak+v))
  for child in $(cat /proc/$1/task/*/children 2>/dev/null); do walk "$child"; done
}
walk "$0"
echo "$peak"' "${arguments[1]}"
    ;;
  start|up)
    service="${arguments[${#arguments[@]}-1]}"
    wait_timeout=300
    for (( index=1; index < ${#arguments[@]}; index++ )); do
      if [[ "${arguments[$index]}" == "--wait-timeout" ]]; then
        wait_timeout="${arguments[$(( index + 1 ))]}"
      fi
    done
    if [[ "$service" == secondbox-runner || "$service" == secondbox-runner-relocation ]]; then
      runner_start "$service"
    else
      "${compose[@]}" "${arguments[@]}"
    fi
    ;;
  stop)
    service="${arguments[${#arguments[@]}-1]}"
    grace=45
    for (( index=1; index < ${#arguments[@]}; index++ )); do
      if [[ "${arguments[$index]}" == "--timeout" ]]; then
        grace="${arguments[$(( index + 1 ))]}"
      fi
    done
    if [[ "$service" == secondbox-runner || "$service" == secondbox-runner-relocation ]]; then
      runner_stop "$service" "$grace"
    else
      "${compose[@]}" "${arguments[@]}"
    fi
    ;;
  logs)
    tail_lines=""
    compose_services=()
    skip_next=false
    for argument in "${arguments[@]:1}"; do
      if [[ "$skip_next" == "true" ]]; then
        tail_lines="$argument"
        skip_next=false
        continue
      fi
      case "$argument" in
        secondbox-runner|secondbox-runner-relocation) runner_logs "$argument" "$tail_lines" ;;
        --tail) skip_next=true ;;
        --no-color|--timestamps) ;;
        -*) ;;
        *) compose_services+=("$argument") ;;
      esac
    done
    if (( ${#compose_services[@]} > 0 )); then
      "${compose[@]}" logs --no-color "${compose_services[@]}"
    fi
    ;;
  *)
    "${compose[@]}" "${arguments[@]}"
    ;;
esac
