# gVisor Task 0H feasibility evidence

Date: 2026-08-25

SecondBox checklist baseline: branch `gvisor-backend-plan`, stacked on
`bb/implement-microsandbox-backend-spike-thr_2iw6thm7r2` (`dd3f8e7`).

## Qualification environment

Task 0H requires a real Linux host without KVM. The qualification host is a QEMU/KVM guest whose
virtualization extensions are masked (`-cpu host,-vmx,-svm`), so the guest kernel cannot load a KVM
module and `/dev/kvm` does not exist. This is a real no-KVM Linux host in every respect the gVisor
backend depends on.

| Role | Evidence |
| --- | --- |
| Qualification host `gvisor-qual` | Debian 13, kernel `6.12.101+deb13-cloud-amd64`, x86_64, 8 vCPUs, 8 GiB, 20 GiB ext4 root; `ls /dev/kvm` → `No such file or directory`; `grep -cE 'vmx\|svm' /proc/cpuinfo` → `0` |
| Build host `deimos` | Linux `7.1.8-arch1-3`, x86_64; QEMU 11.1.0 launches the qualification VM unprivileged; probe development runs used `-allow-kvm-host -rootless` and are not qualification evidence |
| VM base image | `debian-13-genericcloud-amd64.qcow2`, SHA-512 verified against the Debian cloud `SHA512SUMS` (`77429b411b39…ae29c2a3`) |

All qualification commands ran as root inside `gvisor-qual` against
`/opt/gvisor-qual/bin/{runsc,secondbox-gvisor-probe,secondbox-gvisor-probe-guest,secondbox-gvisor-probe-agent}`
with a fresh work directory per run. The invocation is the `test-gvisor-probe` recipe body:

```text
secondbox-gvisor-probe -runsc .../runsc -guest .../secondbox-gvisor-probe-guest \
  -agent .../secondbox-gvisor-probe-agent -work .../work all
```

## Pinned inputs

| Input | Exact evidence |
| --- | --- |
| gVisor `runsc` | `release-20260817.0`, spec 1.2.1, x86_64; SHA-512 `84936438d583ec976800f464e75a83e1515f0890b451b9b4db219c4472b54ca9b106a6772ee683f1e64cce2128871d7637b14d800591f8451b8137f6c39fb2ef`; fetched and verified by `runner/scripts/fetch-runsc.sh` from the immutable tagged release URL |
| Probe sources | `runner/gvisor-probe` (probe, guest, agentharness), built with Go `1.26.6`, `CGO_ENABLED=0`, via `just build-gvisor-probe` |
| Guest-agent implementation | production `runner/internal/guest` and `runner/internal/guestprotocol` from this branch, served by the probe agent harness on Unix-socket listeners |
| Runner-side drivers | production `runner/internal/firecracker` `GuestProtocolSession` operation code (exec, streaming, file, PTY), negotiated by the probe over a plain UDS dial |
| Toolchain-compat fixture | `docker.io/library/alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce` (the microsandbox flat-root base), exported rootfs tar SHA-256 `3cc76732a9530b650e171ab8c8937a126275636a08f768aadf727068ef923092`; battery script `runner/gvisor-probe/toolchain-smoke.sh` |

## Bounded outcomes

Complete probe suite (`all`), qualification host, exit 0:

```text
proof=sandbox-boot-exit boot_millis=81 marker=ok propagated_exit_code=7 status=passed
proof=sandbox-graceful-stop stop_millis=0 survivors=0 status=passed
proof=sandbox-parent-death teardown_millis=1 survivors=0 stale_record=reconciled status=passed
proof=sandbox-forced-kill kill_millis=1 survivors=0 stale_record=reconciled status=passed
proof=cgroup-cpu-quota usage_micros=3037391 bound_micros=6000000 workers=4 quota_cpus=1 status=passed
proof=cgroup-memory-limit limit_bytes=134217728 outcome=exit_status_137 status=passed
proof=workspace-descriptor-mount image_bytes=268435456 uuid=31e40cd45f5a4b54a06e0123456789ab inode_stable=true marker=durable status=passed
proof=workspace-enospc image_bytes=67108864 guest_outcome=enospc e2fsck=clean status=passed
proof=agent-negotiation generation=1 features=5 transport=host-uds status=passed
proof=agent-buffered-exec stdout_bytes=17 exit_code=0 status=passed
proof=agent-streaming-exec echoed_bytes=25 exit_code=0 status=passed
proof=agent-file-roundtrip bytes=4096 host_visible=true status=passed
proof=agent-pty output_bytes=20 resize=sent exit_code=0 status=passed
proof=agent-port-relay guest_port=7777 relayed_bytes=18 status=passed
proof=agent-shutdown signal=SIGTERM status=passed
proof=network-deny-all agent_transport=alive targets_blocked=2 status=passed
proof=network-allow-list allowed=tgt-10.201.0.11 denied_domain=true denied_private=true denied_metadata=true status=passed
proof=network-dns-change new_pin=tgt-10.201.0.13 old_pin_revoked=true status=passed
```

Performance observations (`performance`), qualification host, exit 0. Observations only; no gate:

```text
proof=perf-cold-start samples=30 p50_millis=60 p95_millis=81 min_millis=43 max_millis=114 all_millis=114,60,53,57,52,63,51,56,43,60,58,64,58,53,55,49,72,68,73,62,58,81,60,64,62,70,59,64,64,64 status=passed
proof=perf-workspace-io directfs=true write_mib_s=688.9 read_mib_s=15683.0 total_mib=128 status=passed
proof=perf-workspace-io directfs=false write_mib_s=755.4 read_mib_s=15915.1 total_mib=128 status=passed
```

Toolchain-compatibility battery on the pinned alpine fixture under
`runsc --network=none --platform=systrap`, exit 0, zero syscall-compatibility failures:

```text
check=shell-pipeline status=ok
check=coreutils status=ok
check=file-roundtrip status=ok
check=tar-gzip status=ok
check=proc-reads status=ok
check=signals status=ok
check=fork-pressure status=ok
check=dd-io status=ok
check=awk-sed status=ok
check=date-env status=ok
check=apk-tools status=ok
summary pass=11 fail=0
```

## Proof mechanics worth restating

- The workspace proof attached an already-open raw-ext4 descriptor by creating the loop device
  through `/proc/self/fd/<n>` of the losetup child, mounted it in an unshared recursively private
  mount namespace, and served the mountpoint into the sandbox through the gofer. The image's device,
  inode, and ext4 UUID were identical before attach, after clean detach (syncfs, umount, loop
  release), and after an independent reopen; the guest marker was durable across a read-only
  re-attach. ENOSPC at image capacity reached the guest as a clean write error and `e2fsck -fn`
  passed afterward.
- The agent proof served the production guest protocol implementation on gofer-passed host Unix
  sockets (`runsc --host-uds=all`) and drove it with the production runner-side operation drivers.
  All five requested features negotiated. The Port relay ran through the agent's port feature
  against a sandbox loopback listener, proving netstack loopback works under `--network=none`.
- The network proof enforced an inet-family nftables egress chain on a routed veth in a
  per-Instance network namespace with NAT egress, with runsc's netstack attached via
  `--network=sandbox`. Deny-all blocked every target while agent execs kept working, the exact
  allow-list admitted only the pinned address and port while denying a disjoint domain, a private
  address, and `169.254.169.254`, and a DNS answer change rotated the pin: the former address was
  revoked and the replacement admitted. This proves the rule family and namespace shape Task 6H's
  shared extraction must render; no bridge-family or ARP rules were used.
- Guest exec working directories are workspace-relative: the agent's `toolWorkspacePath` rejects
  absolute paths, so the backend must send `.`-style cwd values.

## Findings for later tasks

- runsc with `--network=none` as root bind-mounts a `null-netns` placeholder inside its state root
  that survives sandbox exit and `runsc delete -force`. The state directory cannot be removed until
  it is unmounted. The Task 4H cleanup stack and startup reconciliation must unmount it; the probe's
  reap path demonstrates the required order.
- directFS on and off measured within a few percent on this VM's ext4-on-virtio storage for
  sequential 128 MiB write/read; the Task 4H composition decision should re-measure on production
  storage before pinning a mode.
- Host-UDS passthrough (`--host-uds=all`) is reliable and is the selected Task 2H transport; the
  veth-TCP fallback was not needed.

## Gate status

Task 0H is **GO / passed**. Every mandatory proof succeeded on the real no-KVM qualification host,
with the forced-kill path recorded separately, cgroup enforcement proven for both bounds, and zero
toolchain-compatibility failures. Tasks 1H through 7H may proceed in order. Tasks 8P and 9P remain
closed until the complete host vertical slice in Task 7H passes.

No dependency source or artifact was published, and no upstream gVisor change was required; the
probe consumes the unmodified pinned release binary.
