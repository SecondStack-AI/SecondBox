# Microsandbox Task 9M macOS port qualification

Date: 2026-08-14

Hosts:

- mini1 (`Apple M1`, `arm64`) for native APFS and Hypervisor.framework qualification.
- deimos (`linux/amd64`) for the post-port Linux regression gate.

Result: pass

This qualification used only local source snapshots, dependency checkouts, builds, containers, and
ad-hoc signatures. It did not create or modify a pull request, issue, release, branch, comment, or
any other external repository state. The SecondBox source checkout was not modified; all SecondBox
changes are confined to the worktree branch.

## Implementation audit

The production source graph is split at the platform boundary:

- `cmd/secondbox-runner/backend_{common,linux,darwin}.go` selects only the supported backend on
  each target. The Darwin dependency graph contains neither Firecracker, the jailer supervisor,
  nor the Linux cgroup package.
- `cmd/secondbox-runner/supervisor_{linux,darwin}.go`,
  `internal/microsandbox/process_{linux,darwin}.go`, and
  `internal/runtimeconfig/composition_{linux,darwin}.go` isolate process supervision and runtime
  composition without changing the Linux implementations.
- A `GOOS=darwin GOARCH=arm64` compile produced an arm64 Mach-O runner. A dependency-list negative
  assertion reported `darwin_source_graph_linux_only_packages=absent`.

The Darwin WorkspaceStore driver uses APFS `clonefile`, descriptor identity checks with
`F_GETPATH`, nonblocking `flock`, file `fsync`, directory `F_FULLFSYNC`, `/dev/fd/<n>` attachment,
and helper-based ext4 formatting. Common WorkspaceStore code continues to own receipts, retention,
atomic publication, generation fencing, UUID validation, snapshots, restores, and relocation.
Native tests additionally proved:

- independent source/destination mutation after real APFS clones;
- sparse formatting and post-import sparse compaction with verified-zero `F_PUNCHHOLE` ranges;
- deterministic ext4 UUIDs and clean `e2fsck` results;
- clone-only snapshot/restore and same-filesystem relocation behavior;
- descriptor/path confinement and cross-process writer exclusion/recovery.

The Linux-created portable fixture is recorded by
`runner/internal/workspacestore/testdata/portable-ext4-v1.json` and was structurally validated on
both hosts:

```text
created on             linux/amd64
logical bytes          8388608
filesystem             ext4
UUID                   9e98fc46-3ca1-5e74-9b6c-bbb0fbf36dd6
raw SHA-256            93a1b0a91ce6a1db192cd9852daf4557688fe66bff5e4c6473bffee2522703a2
transport SHA-256      e800ddacbeff4dafcd70cff22370f77ddfbf39ff56285abff7fb72028f8955ec
```

This is a raw-image structural-compatibility proof, not a claim that cross-architecture live
Sandbox movement is portable relocation.

The operator-owned arm64 fixtures are
`runner/deploy/microsandbox-macos-arm64.resources.json` and
`runner/deploy/microsandbox-macos-arm64.materialization.json`. They define an explicit experimental
arm64 RunnerPool, Profile revision, exact materialization tuple, launch artifacts, and `cold_boot`
capability without changing the existing amd64 standard Profile semantics. Fixture tests validate
their schema and identity.

Packaging is repository-owned and explicit:

- `runner/scripts/build-microsandbox-macos.sh` checks the exact clean Microsandbox revision, trees,
  patch, locks, libkrunfw revision, kernel tarball, and OCI image digests before building a new
  output directory atomically.
- `runner/deploy/microsandbox-hypervisor.entitlements.plist` and
  `runner/scripts/sign-microsandbox-macos.sh` sign and strictly verify the runner, helper, and
  firmware. The helper's effective entitlements contain both Hypervisor.framework and disabled
  library validation.
- The helper loads the operator-pinned bundle library rather than linking a global libkrunfw.
  Qualification rejects Mach-O load commands containing `libkrunfw`, `/Users`, `/opt/homebrew`, or
  `/usr/local`; the retained final helper links only system libraries/frameworks.
- `internal/microsandbox/readiness_darwin.go` fails on a wrong architecture, absent Hypervisor
  support, missing or invalid signatures/entitlements, missing bundle artifacts, an incompatible
  manifest, APFS/descriptor failures, or network-engine mismatch, and cleans its short canonical
  `/tmp` probe root.
- The runner/helper control channel is an inherited socketpair. Darwin uses its own process-group
  supervision and the qualification forces short `/tmp` paths to avoid Unix socket length and
  `/var` versus `/private/var` identity instability.
- `docs/operations/microsandbox-macos.md` documents a separate experimental local installation and
  run path. It does not alter or replace the qualified Linux Firecracker installer.

## Final local build

The final clean build is retained on mini1 at:

```text
/Users/alex/Developer/microsandbox-task9m-build3
```

Its SecondBox input snapshot matched the worktree's 38 changed paths byte-for-byte before the
build. The builder started from a separate clean local Microsandbox checkout at the pinned
revision, applied the reviewed local patch, and rejected dirty or mismatched input.

```text
Microsandbox commit           5b335537afad433ad2c0308cb54de13b7015b4e7
Microsandbox upstream tree    dc506dffd600fcea281bd4ebfc924e1b31afcb2a
Microsandbox patched tree     daf8457b13e5f124a63e23a12edbd8482d7da43a
Microsandbox Cargo.lock       7827c5aad40cfc4ab36be6aba3bc4c0d923e525c50fc4b54741776bcf13b95c8
SecondBox patch               4dd2878ec1f760821a6ebd5f23fef6e767382664db2169676e446045f8674756
helper Cargo.lock             72ba8a7f40cc17eb75386425cdb387c46cb394913d30d51b811f08e9f14d681c
libkrunfw commit              21cb6dce19a615f63e41ecb913334d18560c1364
kernel tarball                194eef900ade82df74ed1d695daa45d03ee4bb415cae4f936a3dbaab2dbbb951
kernel builder image          fedora@sha256:6c75d5bf57cb0fa5aa4b92c6a83c86c791644496d9ac230de7711f5b8ec3b898
rootfs image                  alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
secondbox-runner              80d4554ecb0437e0670a65fb299cbcb3233a6f3d0efa274442eb1e7a6de1437c
secondbox-microsandbox-helper bd3fb5d2ebfdac12371f642ebd7082f30b18d561650ed3efb75f5278c684495f
libkrunfw.5.dylib             bc76e1fb4c6438dce02bc53aae7a204be40571eecb3478963d0011f8b485c05f
agentd                        31b3b2f02a7f233d62336001deb873104d226cc8b00792d629801a1652ed0a50
```

The retained `libkrunfw.dylib` is a relative symlink to `libkrunfw.5.dylib`. Strict signature
verification passed for the runner, helper, and dylib. The helper's effective entitlements are:

```text
com.apple.security.hypervisor = true
com.apple.security.cs.disable-library-validation = true
```

Host/tool inputs were macOS 15.1.1 (24B91), Darwin 24.1.0, APFS,
`kern.hv_support: 1`, Rust/Cargo 1.95.0, Go 1.25.13, Docker client 27.4.0, and local Docker server
27.3.1.

The clean builder also passed 14 helper unit/property tests and two inherited-descriptor/lifecycle
process tests. Its bounded log is retained locally as
`/Users/alex/Developer/microsandbox-task9m-build3.log`.

## Native qualification

The exact retained build passed both fail-rather-than-skip native gates:

```text
SECONDBOX_MICROSANDBOX_MACOS_BUILD=/Users/alex/Developer/microsandbox-task9m-build3 \
  just test-workspacestore-macos
ok github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore 5.524s

SECONDBOX_MICROSANDBOX_MACOS_BUILD=/Users/alex/Developer/microsandbox-task9m-build3 \
  just test-microsandbox-macos
ok github.com/SecondStack-AI/SecondBox/runner/cmd/secondbox-runner 0.265s [no tests to run]
ok github.com/SecondStack-AI/SecondBox/runner/internal/microsandbox 12.100s
```

The second command performed real Hypervisor.framework boots and covered buffered and streaming
exec, stdin and caller credit, cancellation and deadlines, output bounds, binary files, PTY, TCP
Port, deny/allow networking, metadata denial, generation fencing, capacity, forced helper death,
bounded evidence, and cleanup. These are backend qualification tests; the complete normal-control-
plane scenario remains the Task 10M gate.

The native logs are retained at:

```text
/Users/alex/Developer/task9m-build3-workspacestore.log
/Users/alex/Developer/task9m-build3-microsandbox.log
```

## Post-port Linux and generated-code gates

On deimos, `/dev/kvm` was revalidated as a character device with runner access. The retained exact
Linux build at `/home/sasha/.bb/thread-storage/microsandbox-build-task7l-reviewed` passed:

```text
just test-workspacestore-linux
ok github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore 0.543s

just test-microsandbox-linux
helper: 13 unit/property tests and 2 process tests passed
ok github.com/SecondStack-AI/SecondBox/runner/cmd/secondbox-runner 0.004s [no tests to run]
ok github.com/SecondStack-AI/SecondBox/runner/internal/microsandbox 10.974s
```

`just verify-generated` also passed with a worktree-owned npm cache. It verified descriptors, Go
bindings and deploy configuration, 133 TypeScript tests, the npm dry-run package, resource apply,
materialization, runtime composition, runner command, WorkspaceStore, and Microsandbox targets.
`git diff --check` passed.

## Corrections discovered by native execution

Fresh native runs exposed and verified fixes for five platform-specific defects:

1. APFS clonefile retained the immutable template's `0400` mode; the clone is now made writable
   only after cloning.
2. macOS e2fsprogs allocated zero-filled ext4 regions; verified-zero ranges are now hole-punched.
3. Relocation import could allocate APFS clusters; imported images are compacted before publish.
4. APFS attached host-only `com.apple.*` provenance xattrs that could not enter the guest root;
   helper root materialization now filters host provenance while preserving portable guest xattrs.
5. Helper evidence names the platform `macos-aarch64`; the Go host diagnostic now maps
   `darwin/arm64` to that canonical helper identity.

The final clean build and both native gates passed only after these corrections. Task 9M passes;
the complete macOS control-plane vertical slice and dual-platform regression closure remain Task
10M.
