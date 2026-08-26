#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# Runs the identical gVisor scenario suite with the runner placed as a
# privileged Kubernetes pod on this node instead of a Compose service. The
# control plane and postgres stay in Compose; a service-control script
# translates runner lifecycle verbs onto pods. Run it on the node itself.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  echo "SecondBox gVisor pod scenario: $*" >&2
  exit 1
}

kubectl_command=(${SECONDBOX_SCENARIO_POD_KUBECTL:-k3s kubectl})
"${kubectl_command[@]}" get nodes >/dev/null 2>&1 ||
  fail "a reachable Kubernetes node is required (set SECONDBOX_SCENARIO_POD_KUBECTL if kubectl differs from 'k3s kubectl')"

if [[ -z "${SECONDBOX_SCENARIO_POD_CONTROL_PLANE_HOST:-}" ]]; then
  SECONDBOX_SCENARIO_POD_CONTROL_PLANE_HOST="$(ip -json route get 1.1.1.1 | jq -er '.[0].prefsrc')" ||
    fail "could not resolve the node address for the control plane; set SECONDBOX_SCENARIO_POD_CONTROL_PLANE_HOST"
fi
export SECONDBOX_SCENARIO_POD_CONTROL_PLANE_HOST
export SECONDBOX_SCENARIO_RUNNER_PLACEMENT=pod

exec "$repo_root/scripts/test-scenario-gvisor.sh"
