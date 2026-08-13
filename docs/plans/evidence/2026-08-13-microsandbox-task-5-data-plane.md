# Microsandbox Task 5L data-plane qualification

Date: 2026-08-13

Host: deimos (`linux/amd64`, real `/dev/kvm`)

Result: pass

## Implemented boundary

- Buffered and streaming Exec use the private helper protocol and agentd, with exact output bounds,
  byte credit, stdin/EOF, channel preservation, deadlines, cancellation, PTY resize, typed spawn
  failures, exit codes, and signals. Explicit command paths are preflighted through the already
  qualified guest filesystem API because the pinned agentd protocol does not otherwise expose a
  failed `execve` as a distinct spawn event.
- File stat, read, write, list, exists, mkdir, and remove are binary-safe and confined beneath
  `/workspace`. Writes verify exact size and SHA-256 before transmission; reads and frames are
  bounded. Canonicalization rejects path and symlink escapes.
- Port opens one generation-fenced guest TCP stream through agentd. Relay is bounded and
  bidirectional, creates no host listener, acknowledges close, and leaves the helper usable for a
  subsequent operation.
- Every operation acquires the complete assignment fence before reaching the helper and rechecks it
  before publishing its terminal. Unknown helper outcomes become provider-neutral infrastructure
  failures. Unexpected helper death publishes one fenced terminal, never a duplicate.
- The provider-neutral conformance fixture is shared by Firecracker and Microsandbox. It covers
  typed spawn failure, exact output exhaustion, cancellation/caller disconnect, stream credit and
  stdout/stderr identity, binary Workspace files, and PTY cancellation. The Linux qualification
  adds stdin, deadline, signal, resize, Port, capacity, fence, helper-death, and duplicate-terminal
  coverage. The public PTY contract has no detach/reattach operation, so none was invented.

## Exact local build

The final bundle was built only from local source into:

```text
/home/sasha/.bb/thread-storage/microsandbox-build-task5l-final2
```

Pinned evidence:

```text
Microsandbox commit       5b335537afad433ad2c0308cb54de13b7015b4e7
Microsandbox source tree  dc506dffd600fcea281bd4ebfc924e1b31afcb2a
Microsandbox patched tree 2bb82ba33e2175cd7574ffa6d058c6968453fa4b
SecondBox patch SHA256    f38294823f2c8e3b8e7918a8c58b48b0c9c7c521874add5d5985af3d4134eb7c
Helper SHA256             80018eb77d585417ed88f02232032647514f17c546af41b00bed50df845dcb99
agentd SHA256             b1a0ba13b7af54df89073af6909f12df737e6553ecd23bc14fcc7e30b2f68a41
msb SHA256                107c7a818d844db51333d014d9869c9331a0fbfde9a8c750492e26eb55accd07
libkrunfw SHA256          858bfd9a63236409a409191a1ea3c69413f4163f49ebd0f3835f68385831481c
probe SHA256              40993d5ff39139ee3ff8c7a608c8949930e44d49d8a5994acd145390b5551def
```

The local builder verified the pinned revision, clean source tree, patch digest, patched tree,
lockfiles, submodule, and packaged artifact digests. It performed no push, fork, PR, issue,
release, or other external repository write.

## Validation

Passed:

```text
just test-microsandbox-linux
cd runner && GOCACHE=/tmp/secondbox-task5-go-cache \
  go test ./internal/runnercontrol/... ./internal/microsandbox/... -count=1
just test-contract
just test-firecracker
GOCACHE=/tmp/secondbox-task5-go-cache npm_config_cache=/tmp/secondbox-npm-cache \
  just verify-generated
GOLANGCI_LINT_CACHE=/tmp/secondbox-golangci-cache \
  GOCACHE=/tmp/secondbox-task5-go-cache just lint
CARGO_HOME=/tmp/secondbox-cargo-home CARGO_TARGET_DIR=/tmp/secondbox-msb-helper-target \
  cargo clippy --manifest-path runner/microsandbox-helper/Cargo.toml \
  --all-targets -- -D warnings
```

The Microsandbox qualification completed against real KVM in 5.614 seconds; helper tests passed 11
unit/property tests and 2 inherited-descriptor process tests. Firecracker's shared data-plane suite
passed with local Firecracker 1.16.1 and signed local fixtures in 26.072 seconds. Firecracker needed
a short `/tmp/fq5` symlink to a same-Btrfs qualification root under `/home/sasha/Developer` because
its Unix socket limit and reflink requirement cannot both be met by the default cross-device `/tmp`
layout.

The plan's literal `./internal/dataplane/...` Go package does not exist. The corrected command above
uses the actual provider-neutral package, `./internal/runnercontrol/...`, and passed. Initial lint,
generated-code, and Firecracker attempts also exposed read-only default caches or missing explicit
test prerequisites; reruns used task-scoped caches and explicit local fixtures and passed without
adding runtime defaults.

No macOS work was started. Task 6L is the next open checklist item.
