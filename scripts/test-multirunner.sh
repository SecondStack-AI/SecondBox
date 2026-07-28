#!/usr/bin/env bash
set -euo pipefail

cat >&2 <<'EOF'
SecondBox multi-runner qualification is unavailable in the extracted baseline.
Required before this gate can pass:
- a versioned runner protocol with authenticated outbound runner connections;
- two independent qualified Linux runner hosts with KVM, TUN/TAP, cgroup v2, and signed artifacts;
- distinct runner identities in one compatible runner pool;
- durable PostgreSQL assignment/fencing state and shared S3-compatible checkpoint storage;
- an automated drain, loss, stale-runner rejection, and cross-runner resume scenario.
EOF
exit 1
