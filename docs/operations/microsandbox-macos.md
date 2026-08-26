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

Local development may use ad-hoc signing only when it is mechanically required to execute the
helper. That does not verify the repository signing workflow or qualify production distribution.
A persistent installation must select its signing identity explicitly and rerun the repository
signing step in an environment that can provide independent signing evidence:

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

The complete required runner environment beyond the backend block above is the following set;
every value is deployment authority with no application default. The scenario service control in
`scripts/scenario-microsandbox-macos-service-control.sh` composes exactly this environment for
the qualification suite and is the executable reference for a working assembly.

| Variable | Value source |
| --- | --- |
| `SECONDBOX_COMPUTE_BACKEND` | Literal `microsandbox`. |
| `SECONDBOX_RUNNER_ID` | The Runner identity declared to the control plane; must match the certificate's `spiffe://secondbox/runner/<runner-id>`. |
| `SECONDBOX_RUNNER_POOL_ID` | The `name` of the dedicated arm64 RunnerPool from the applied resources file. |
| `SECONDBOX_RUNNER_SOFTWARE_VERSION` | The exact source identity of the deployed runner build (release version or commit). |
| `SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS` / `SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME` | The control plane's Runner gRPC endpoint and its server certificate name. |
| `SECONDBOX_RUNNER_CREDENTIAL` | The pre-shared Runner enrollment credential configured on the control plane. |
| `SECONDBOX_RUNNER_CLIENT_CERTIFICATE` / `SECONDBOX_RUNNER_CLIENT_KEY` / `SECONDBOX_RUNNER_CONTROL_PLANE_CA` | The installed identity files from the issuance procedure above. |
| `SECONDBOX_RUNNER_ENABLED_FEATURES` | The feature list the control plane enables (for example `exec-streaming,file-streaming,pty,evidence,local-workspace,port-proxy`). |
| `SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS` / `SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL` | Operator-selected heartbeat cadences (the guest interval is a Go duration of at most 60s). |
| `SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS` / `SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS` | The data-plane listener and the address the control plane's clients can reach it at. |
| `SECONDBOX_RUNNER_LOG_DIR` / `SECONDBOX_RUNNER_LOG_PATH` | Operator-owned log directory and JSONL log file beneath it. |
| `SECONDBOX_RUNNER_WORKSPACE_ROOT` | The APFS WorkspaceStore root from this document. |
| `SECONDBOX_RUNNER_SANDBOX_MAX_VCPUS` / `..._MAX_MEMORY_MIB` / `..._MAX_DISK_MIB` / `..._MEMORY_BUDGET_MIB` | Per-Sandbox ceilings and the host memory budget the operator allocates. |
| `SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX` / `..._GLOBAL` / `..._STARTS` / `..._WORKSPACE_CREATES` / `..._OPERATIONS_GLOBAL` | Concurrency bounds; global instance and operation bounds must not exceed the backend maxima above. |
| `SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES` | The file data-plane transfer bound. |
| `SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT` / `..._WARNING_PERCENT` / `..._ADMISSION_DENY_PERCENT` | WorkspaceStore pressure thresholds in ascending order. |

Run the native runner as a dedicated unprivileged identity; Darwin rejects a
root runner because Hypervisor.framework and APFS clonefile do not require Linux's KVM, cgroup,
namespace, or TAP authority. Never run the control plane with Hypervisor or WorkspaceStore access.

The runner uses inherited socketpairs rather than pathname Unix sockets. `TMPDIR=/tmp` keeps
ephemeral runtime paths short, and identity checks tolerate macOS's `/var` to `/private/var`
canonicalization while still comparing the held descriptor's inode. The WorkspaceStore alone
resolves host paths and passes `/dev/fd/<n>` attachments to compute.

## Known limitations

Workspace file writes never follow a leaf symlink, but the guest agent's filesystem protocol is
pathname-only: a process already executing inside the guest can race a parent-directory swap
against a write. The race is contained inside the guest (such a process can already write guest
files directly); handle-relative beneath semantics require an agent protocol revision.

The helper serves one request at a time over its single control channel. An open Port tunnel
therefore serializes every other Exec, file, and Port operation on the same Sandbox until the
tunnel closes; concurrent operations queue rather than fail. Lifting this requires a helper
protocol revision and a re-pinned helper build, and is out of scope for the experimental
backend.

## Materialization and digest verification

Every value the runner verifies is computed from the operator's reviewed build, never taken on
faith from a document:

- Compute the flat-root digest with the repository tool and record it as `flatRootDigest`:

  ```sh
  cd runner && go run ./cmd/secondbox-flat-root-digest /opt/secondbox-microsandbox/rootfs
  ```

- Record each launch artifact's SHA-256 (`shasum -a 256` on macOS) for the helper and `agentd`
  entries in `launchArtifacts`.
- Compute the canonical materialization digest over the reviewed manifest's compact JSON and pin
  it in the environment:

  ```sh
  printf 'sha256:%s\n' "$(jq --compact-output --join-output . \
    /etc/secondbox/microsandbox-arm64.materialization.json | shasum -a 256 | awk '{print $1}')"
  export SECONDBOX_MICROSANDBOX_MATERIALIZATION_DIGEST=sha256:...
  ```

The runner revalidates the manifest digest and every launch artifact before it advertises, and
refuses to start on any mismatch. Repeat the computation after every change to the reviewed
build; the digest is the identity, not a cache.

## Enrollment and credential issuance

`secondbox-deploy runner-init` declares, signs, and renders Firecracker-shaped Linux Runners
only; it does not yet render a Microsandbox declaration or a macOS environment. For a macOS
Runner, issue the identity manually with the deployment's Runner CA — the same authority
`runner-init` uses: sign a client certificate whose URI SAN is
`spiffe://secondbox/runner/<runner-id>`, using the deployment's `runner-ca.crt` and
`runner-ca.key`, and provision the pre-shared Runner enrollment credential the control plane is
configured with. Copy the certificate, key, and CA certificate to the macOS host as an explicit
operator action, install them under an operator-owned root (for example
`/opt/secondbox-runner-identity`), and point `SECONDBOX_RUNNER_CLIENT_CERTIFICATE`,
`SECONDBOX_RUNNER_CLIENT_KEY`, and `SECONDBOX_RUNNER_CONTROL_PLANE_CA` at the installed files.
The complete environment is composed from this document rather than rendered by a tool. Runner
and application credentials remain different authorities; never reuse an application bearer
credential for enrollment.

## Persistent service management

Run the native runner as a `launchd` daemon under the dedicated unprivileged identity. Install an
operator-reviewed plist (for example
`/Library/LaunchDaemons/ai.secondstack.secondbox.runner.plist`) that states the exact
`secondbox-runner` program path, the complete environment from this document, `UserName` set to
the dedicated identity, `KeepAlive` for supervised restart, and `StandardOutPath`/
`StandardErrorPath` under the operator-owned log root. Manage it with
`launchctl bootstrap system <plist>`, `launchctl bootout system/<label>`, and
`launchctl kickstart -k system/<label>` for a supervised restart. The environment file produced
by the handoff is deployment authority: regenerate the plist's environment block from it rather
than editing values in place, and keep the identity directory readable only by the runner's
user.

## Qualification before enrollment

```sh
export SECONDBOX_MICROSANDBOX_MACOS_BUILD=/absolute/path/to/secondbox-microsandbox-macos
just test-workspacestore-macos
just test-microsandbox-macos
```

These commands fail rather than skip when Hypervisor.framework execution, APFS clonefile, e2fs
tooling, the materialization, inherited descriptors, the network engine, or cleanup are
unavailable. They may exercise a locally or ad-hoc signed binary as a mechanical prerequisite, but
they do not qualify repository or production signing. Task 10M adds the full control-plane scenario
and cross-platform regression gate. Passing this document's checks does not remove the experimental
label.
