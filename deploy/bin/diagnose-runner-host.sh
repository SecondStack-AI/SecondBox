#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ "$#" -ne 1 ]]; then
  echo "Usage: deploy/bin/diagnose-runner-host.sh OUTPUT.txt" >&2
  exit 2
fi

output_path="$1"
if [[ -e "$output_path" ]]; then
  echo "Refusing to overwrite runner diagnostic: $output_path" >&2
  exit 1
fi

: "${SECONDBOX_RUNNER_DIAGNOSTIC_JOURNAL_LINES:?set SECONDBOX_RUNNER_DIAGNOSTIC_JOURNAL_LINES}"
: "${SECONDBOX_RUNNER_SYSTEMD_UNIT:?set SECONDBOX_RUNNER_SYSTEMD_UNIT}"
if [[ ! "$SECONDBOX_RUNNER_DIAGNOSTIC_JOURNAL_LINES" =~ ^[0-9]+$ ]] ||
   (( SECONDBOX_RUNNER_DIAGNOSTIC_JOURNAL_LINES < 1 ||
      SECONDBOX_RUNNER_DIAGNOSTIC_JOURNAL_LINES > 10000 )); then
  echo "SECONDBOX_RUNNER_DIAGNOSTIC_JOURNAL_LINES must be from 1 through 10000" >&2
  exit 1
fi

{
  echo "SecondBox runner host diagnostic"
  echo
  uname -a
  echo
  systemctl show "$SECONDBOX_RUNNER_SYSTEMD_UNIT" \
    --property ActiveState \
    --property SubState \
    --property MainPID \
    --property ExecMainStatus
  echo
  ls -l /dev/kvm
  echo
  stat -fc 'cgroup filesystem: %T' /sys/fs/cgroup
  echo
  df -hT
  echo
  journalctl \
    --unit "$SECONDBOX_RUNNER_SYSTEMD_UNIT" \
    --lines "$SECONDBOX_RUNNER_DIAGNOSTIC_JOURNAL_LINES" \
    --no-pager
} >"$output_path"
chmod 600 "$output_path"
echo "Created bounded runner diagnostic: $output_path"
