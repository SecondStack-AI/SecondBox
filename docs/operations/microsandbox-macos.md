# Experimental Microsandbox runner on Apple Silicon macOS

The native macOS runner is experimental. It is a separate, operator-managed Runner deployment;
it does not alter or replace the qualified Linux Firecracker installer, container, systemd units,
network setup, or standard amd64 Profiles.

## Host and build contract

Use an Apple Silicon host with Hypervisor.framework enabled and an APFS volume for the complete
WorkspaceStore root. Install Xcode Command Line Tools, Homebrew `e2fsprogs`, Go, Rust, Docker plus
Colima, `protobuf`, and `just`. No external source write is required.

Build from the reviewed local Microsandbox checkout and a new output path:

```sh
export PATH=/opt/homebrew/bin:/opt/homebrew/opt/rustup/bin:/opt/homebrew/opt/colima/bin:/usr/bin:/bin:/usr/sbin:/sbin
just build-microsandbox-probe-macos \
  /absolute/path/to/clean/microsandbox-5b335537 \
  /absolute/path/to/new/secondbox-microsandbox-macos
```

The builder rejects a dirty or wrong-revision dependency, checks the reviewed patch and lock
digests, rebuilds the pinned libkrunfw input, builds `secondbox-runner` and the helper for
`darwin/arm64`, and creates this bundle:

```text
runtime/bin/secondbox-runner
runtime/bin/secondbox-microsandbox-helper
runtime/bin/agentd
runtime/lib/libkrunfw.5.dylib
rootfs/
build-evidence.txt
signing-evidence.txt
```

Local qualification uses ad-hoc signing. A persistent installation must select its signing
identity explicitly and rerun the repository signing step:

```sh
runner/scripts/sign-microsandbox-macos.sh \
  --bundle /absolute/path/to/secondbox-microsandbox-macos/runtime \
  --identity 'OPERATOR-SELECTED-CODESIGN-IDENTITY'
```

The helper—not the unprivileged control plane—receives the Hypervisor entitlement. The signing
step verifies the effective entitlement, every code signature, and rejects mutable global or
user-specific Mach-O library paths. The runner passes the exact bundled libkrunfw path to the
helper; neither component uses a global loader search path or `MSB_HOME`.

## Operator-owned resources

Copy and review both explicit fixtures:

- `runner/deploy/microsandbox-macos-arm64.resources.json` declares a separate arm64 RunnerPool and
  Profile.
- `runner/deploy/microsandbox-macos-arm64.materialization.json` shows the private backend
  materialization shape.

The repeated numeric SHA-256 values in the fixtures are deliberate non-release fixture identities.
Replace every runtime, toolchain, source OCI, flat-root, and launch-artifact digest with the exact
values from the operator's reviewed assets and `build-evidence.txt`. Compute and pin the canonical
materialization digest. Do not attach an arm64 runner to `standard-amd64` or change a published
standard Profile revision.

Preview and apply the reviewed public resources with the normal resource engine:

```sh
secondbox resources check --file /absolute/path/to/reviewed-macos.resources.json
secondbox resources apply --file /absolute/path/to/reviewed-macos.resources.json
```

Enroll the runner into that exact pool with a separately issued runner credential. Runner and
application credentials are different authorities.

## Explicit native runner environment

The following values are examples of required names, not runtime defaults. Materialize every path,
identity, capacity, and credential for the actual installation. Keep the runtime and WorkspaceStore
on operator-owned roots; never place them under a mutable per-user Microsandbox home.

```sh
export PATH=/opt/homebrew/opt/e2fsprogs/bin:/opt/homebrew/opt/e2fsprogs/sbin:/usr/bin:/bin:/usr/sbin:/sbin
export TMPDIR=/tmp
export SECONDBOX_COMPUTE_BACKEND=microsandbox
export SECONDBOX_RUNNER_WORKSPACE_ROOT=/Users/Shared/SecondBox/workspaces
export SECONDBOX_RUNNER_LOG_DIR=/Users/Shared/SecondBox/log
export SECONDBOX_RUNNER_LOG_PATH=/Users/Shared/SecondBox/log/runner.jsonl
export SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE=/opt/secondbox-microsandbox/runtime/bin/secondbox-microsandbox-helper
export SECONDBOX_MICROSANDBOX_LIBKRUNFW_PATH=/opt/secondbox-microsandbox/runtime/lib/libkrunfw.5.dylib
export SECONDBOX_MICROSANDBOX_AGENTD_PATH=/opt/secondbox-microsandbox/runtime/bin/agentd
export SECONDBOX_MICROSANDBOX_FLAT_ROOT_PATH=/opt/secondbox-microsandbox/rootfs
export SECONDBOX_MICROSANDBOX_MATERIALIZATION_PATH=/etc/secondbox/microsandbox-arm64.materialization.json
export SECONDBOX_MICROSANDBOX_MATERIALIZATION_DIGEST=sha256:OPERATOR_CANONICAL_DIGEST
export SECONDBOX_MICROSANDBOX_MAXIMUM_VCPUS=8
export SECONDBOX_MICROSANDBOX_MAXIMUM_MEMORY_BYTES=8589934592
export SECONDBOX_MICROSANDBOX_MAXIMUM_DISK_BYTES=34359738368
export SECONDBOX_MICROSANDBOX_MAXIMUM_INSTANCES=4
export SECONDBOX_MICROSANDBOX_MAXIMUM_OPERATIONS=32
export SECONDBOX_MICROSANDBOX_WORKSPACE_TEMPLATE_CAPACITY_BYTES=8589934592
```

Also set every required runner protocol address, RunnerPool ID, runner identity, mTLS certificate,
private key, CA, enabled feature, and evidence setting. These are deployment authority and have no
application defaults. Run the native runner as the dedicated privileged runner identity; never run
the control plane with Hypervisor or WorkspaceStore access.

The runner uses inherited socketpairs rather than pathname Unix sockets. `TMPDIR=/tmp` keeps
ephemeral runtime paths short, and identity checks tolerate macOS's `/var` to `/private/var`
canonicalization while still comparing the held descriptor's inode. The WorkspaceStore alone
resolves host paths and passes `/dev/fd/<n>` attachments to compute.

## Qualification before enrollment

```sh
export SECONDBOX_MICROSANDBOX_MACOS_BUILD=/absolute/path/to/secondbox-microsandbox-macos
just test-workspacestore-macos
just test-microsandbox-macos
```

These commands fail rather than skip when Hypervisor.framework, APFS clonefile, code signatures,
helper entitlement, e2fs tooling, the materialization, inherited descriptors, the network engine,
or cleanup are unavailable. Task 10M adds the full control-plane scenario and cross-platform
regression gate. Passing this document's checks does not remove the experimental label.
