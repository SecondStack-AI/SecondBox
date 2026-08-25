#!/bin/sh
set -u
pass=0; fail=0
check() {
  label="$1"; shift
  if out=$(sh -c "$*" 2>&1); then
    echo "check=$label status=ok"
    pass=$((pass+1))
  else
    echo "check=$label status=FAILED exit=$? output=$out"
    fail=$((fail+1))
  fi
}
check shell-pipeline "echo hello | tr a-z A-Z | grep -q HELLO"
check coreutils "ls -la / >/dev/null && stat /etc/os-release >/dev/null && du -s /usr >/dev/null && find /etc -name os-release | grep -q os-release"
check file-roundtrip "cp /etc/os-release /tmp/f && cmp /etc/os-release /tmp/f && mv /tmp/f /tmp/g && rm /tmp/g"
check tar-gzip "tar -C /etc -cf /tmp/t.tar os-release && gzip /tmp/t.tar && gunzip /tmp/t.tar.gz && tar -tf /tmp/t.tar | grep -q os-release"
check proc-reads "cat /proc/self/status >/dev/null && cat /proc/cpuinfo >/dev/null && cat /proc/meminfo >/dev/null && ps aux >/dev/null"
check signals "sh -c 'trap \"exit 42\" TERM; kill -TERM \$\$; sleep 1' ; [ \$? -eq 42 ]"
check fork-pressure "i=0; while [ \$i -lt 50 ]; do sh -c true & i=\$((i+1)); done; wait"
check dd-io "dd if=/dev/zero of=/tmp/dd.bin bs=1M count=8 2>/dev/null && dd if=/tmp/dd.bin of=/dev/null bs=1M 2>/dev/null && rm /tmp/dd.bin"
check awk-sed "printf 'a 1\nb 2\n' | awk '{s+=\$2} END {print s}' | grep -qx 3 && echo abc | sed s/b/X/ | grep -qx aXc"
check date-env "date -u >/dev/null && env >/dev/null && uname -a >/dev/null"
check apk-tools "apk --version >/dev/null"
echo "summary pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
