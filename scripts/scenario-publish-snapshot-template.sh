#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# Publish one snapshot-resume template into the scenario Runner's cache before
# the Runner starts. This is the interim operator flow the plan records: a
# Runner does not build its own templates yet, so nothing advertises
# snapshot-resume until an operator materializes one from the Runner's own
# verified bundle and configuration.
#
# The build's private state lives under the build root and is removed here, so
# the Runner's Workspace root contains only the published cache directory.

build_root="${SECONDBOX_SNAPSHOT_TEMPLATE_PUBLISH_BUILD_ROOT:?build root is required}"
report="${SECONDBOX_SNAPSHOT_TEMPLATE_PUBLISH_REPORT:?report path is required}"
cgroup_parent="${SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT:?cgroup parent is required}"

cleanup() {
  local status=$?
  # The jailer creates one cgroup per Instance under the run's parent cgroup and
  # never removes it.
  if [[ -d "/sys/fs/cgroup/$cgroup_parent" ]]; then
    find "/sys/fs/cgroup/$cgroup_parent" -depth -type d -exec rmdir {} + || true
  fi
  rm -rf -- "$build_root" || true
  exit "$status"
}
trap cleanup EXIT

rm -rf -- "$build_root"
mkdir -p -- "$build_root/tmp"

/opt/secondbox-snapshot-template-publish.test \
  -test.run '^TestSmokePublishSnapshotResumeTemplate$' \
  -test.count=1 \
  -test.timeout=45m \
  -test.v

cp -- "$SECONDBOX_SNAPSHOT_TEMPLATE_PUBLISH_OUTPUT" "$report"
chmod 0644 -- "$report"
