# Microsandbox Task 8M Apple Silicon feasibility

Date: 2026-08-14

Host: mini1 (`Apple M1`, `arm64`)

Result: pass

This qualification used only local source checkouts, builds, containers, and an ad-hoc signed
runtime. No external repository write, pull request, issue, comment, release, or push occurred.
The source checkout remained clean at the required upstream revision.

## Hard host gate

- macOS 15.1.1 (24B91), Darwin 24.1.0, `arm64`.
- `kern.hv_support: 1`; the VM proof performed real Hypervisor.framework boots.
- The proof and output roots are on APFS (`/dev/disk3s1s1`).
- Rust and Cargo are 1.95.0 for `aarch64-apple-darwin`.
- Docker client 27.4.0 and local Colima server 27.3.1 supplied build containers only.
- `msb` was ad-hoc signed with `com.apple.security.hypervisor` and
  `com.apple.security.cs.disable-library-validation` entitlements. `libkrunfw.5.dylib` was
  separately ad-hoc signed.

## Exact local inputs and outputs

```text
Microsandbox commit          5b335537afad433ad2c0308cb54de13b7015b4e7
Microsandbox upstream tree   dc506dffd600fcea281bd4ebfc924e1b31afcb2a
Microsandbox patched tree    daf8457b13e5f124a63e23a12edbd8482d7da43a
Microsandbox Cargo.lock      7827c5aad40cfc4ab36be6aba3bc4c0d923e525c50fc4b54741776bcf13b95c8
SecondBox patch              4dd2878ec1f760821a6ebd5f23fef6e767382664db2169676e446045f8674756
probe Cargo.lock             95f0107a1c27f7ad079012a919213207b4256950b73aec33ee624ed33c4638a7
helper Cargo.lock            72ba8a7f40cc17eb75386425cdb387c46cb394913d30d51b811f08e9f14d681c
libkrunfw commit             21cb6dce19a615f63e41ecb913334d18560c1364
kernel tarball               194eef900ade82df74ed1d695daa45d03ee4bb415cae4f936a3dbaab2dbbb951
kernel builder image         fedora@sha256:6c75d5bf57cb0fa5aa4b92c6a83c86c791644496d9ac230de7711f5b8ec3b898
rootfs image                 alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
agentd                       31b3b2f02a7f233d62336001deb873104d226cc8b00792d629801a1652ed0a50
msb                          976a8e356f01ef51a9027f7cf877387a71f10e2001a78b985ff2857acf251dde
libkrunfw.5.dylib            bc76e1fb4c6438dce02bc53aae7a204be40571eecb3478963d0011f8b485c05f
probe                        c1af6b5a846926a727f274a80c6b4366de8351b9c04389e79e8ccbb9ba1ad6f7
secondbox-microsandbox-helper abfad20a03f70c90d05937bf49eeb493a3d771a4946adb8446a8adbc5a16fca6
```

The final build is `/Users/alex/Developer/microsandbox-task8m-build4`. The builder rejected a
dirty or wrong-revision source, checked the upstream and patched Git trees, checked all lock and
patch digests, rebuilt the guest kernel and native dylib, ran locked tests offline after explicit
locked fetches, signed the runtime, and wrote `build-evidence.txt` before publishing the new output
directory atomically.

The local patch was extended during Task 8M to preserve inherited Unix descriptor paths instead
of canonicalizing `/dev/fd/<n>` into an invalid sibling path. The same regression accepts
`/proc/self/fd/<n>` on Linux and passed locally against the final patched source. Task 10M reruns
the complete Linux qualification against this final patch before cross-platform qualification can
pass.

## Real boot proofs

The complete proof was run against the clean build above in a new APFS directory:

```text
proof=ext4-descriptor-uuid source_inode=103893316 clone_inode=103893317 logical_bytes=268435456 source_allocated_bytes=4243456 clone_allocated_bytes=4243456 source_uuid=41414141414141414141414141414141 clone_uuid=52525252525252525252525252525252 status=passed
proof=vm-descriptor-lifecycle inode=103893320 vmm_pid=49826 buffered=buffered-ok streamed=stream-astream-b ping_rtt_micros=2918 shutdown_millis=37 marker=secondbox-task0l-marker lifecycle_pid=49836 lifecycle_shutdown_millis=104 lifecycle_marker=lifecycle-eof-marker force_kill_pid=49841 force_kill_millis=53 status=passed
proof=network-policy allowed_bytes=559 denied_domain=true denied_private=true denied_metadata=true deny_all=true dns_change=true status=passed
```

The ext4 proof formats through the parent-held descriptor, checks descriptor/inode identity before
and after formatting, performs a real APFS `clonefile`, verifies equal sparse logical size, mutates
source and clone independently, rewrites the clone UUID, validates both UUIDs, and runs `e2fsck`
against both images. The output shows distinct inodes, 256 MiB logical images, sparse allocation,
and distinct deterministic UUIDs.

The VM proof starts control work before entering the blocking VMM call. While that call owns its
thread, the proof runs buffered and streaming commands concurrently with control pings, persists a
marker through the inherited `/dev/fd/<n>` Workspace descriptor, and shuts down through the
control channel in 37 ms. The parent descriptor and inode remain stable and the stopped image
passes `e2fsck`.

A separate real boot closes the inherited lifecycle-pipe writer and observes clean VMM exit and a
flushed marker in 104 ms. A third boot exercises the deadline force-kill mechanism separately and
observes exit in 53 ms.

The network proof permits the exact representative allowed flow and rejects a disallowed domain,
private target, metadata target, and deny-all traffic. It also proves the DNS-change/rebinding
defense. Any false result makes the probe fail.

## Validation

Passed on mini1:

```text
just build-microsandbox-probe-macos \
  /Users/alex/Developer/microsandbox-task8m-5b335537 \
  /Users/alex/Developer/microsandbox-task8m-build4

just test-microsandbox-probe-macos \
  /Users/alex/Developer/microsandbox-task8m-build4 \
  /Users/alex/Developer/microsandbox-task8m-proof4
```

The build stages `runner/microsandbox-probe` beneath the exact patched Microsandbox source and
runs the plan's Cargo validation there with `--locked`; its descriptor-identity test passed 1/1.
The independently locked helper suite passed 13/13 unit/property tests and 2/2 inherited-
descriptor/lifecycle process tests. This staged location is required because the probe's path
dependencies intentionally resolve within the exact Microsandbox source tree.

`git diff --check` passed in the SecondBox worktree after the final builder correction.

Two non-qualification clean attempts exposed prerequisites and did not produce published build
directories. The first inherited `CARGO_NET_OFFLINE=true` into its fetch phase; the builder now
forces locked fetches online and all compilation/tests offline. The second exhausted the host disk
while archiving Rust output; only the superseded task-owned build and helper scratch directories
were removed, increasing free space from 6.7 GiB to 21 GiB. Build4 then completed from a wholly new
staging directory. Neither failed staging tree was reused.

Task 8M passes. This proves the Linux mechanisms are feasible on this Apple Silicon host; it does
not qualify production macOS runner composition or remove the experimental label.
