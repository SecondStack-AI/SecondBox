# Task 4L Linux assignment backend evidence

Task 4L passed on `deimos` on 2026-08-13. All work used the SecondBox worktree and local,
digest-pinned Microsandbox sources. No external repository was mutated.

## Outcome

- Strict runner composition selects exactly one of `firecracker` or `microsandbox`; absence and
  unknown values fail configuration.
- The Microsandbox backend reports its private kind, build identity, materialization evidence,
  integer capacity, and measured startup timing through the existing runner protocol.
- One Rust helper owns each libkrun VM. The runner retains the complete assignment fence,
  generation-bound Workspace attachment, exclusive lock, capacity reservation, and lifecycle
  pipe until fencing completes.
- Ready is published only after agentd protocol generation 6 is negotiated and `/workspace`
  passes a write/read/remove probe.
- Fencing atomically rejects operations, cancels active contexts, requests guest shutdown,
  enforces a bounded exit, reaps the helper, closes inherited resources, and releases the
  Workspace. An unexpected post-ready helper exit produces one terminal event.
- The backend implements the existing provider-neutral exec, file, PTY, and Port interfaces;
  Task 5L retains the remaining race, credit, and terminal conformance work.

## Local build

The reproducible builder produced:

`/home/sasha/.bb/thread-storage/microsandbox-build-task5l`

It used Microsandbox commit `5b335537afad433ad2c0308cb54de13b7015b4e7`, the reviewed local
descriptor/UUID patch, `msb_krun` 0.1.30, libkrunfw 5.6.1, and an Alpine root containing the local
agentd build at `/init.secondbox-agentd`.

## Validation

- `cd runner && go test ./cmd/secondbox-runner ./internal/microsandbox/... -count=1` — passed.
- `just test-microsandbox-linux` with the build above and `/dev/kvm` — passed in 5.149 seconds.
  The real-KVM test booted agentd, probed the Workspace, ran buffered and streaming commands,
  round-tripped binary file bytes, rejected concurrent Instance capacity, fenced cleanly, and
  proved exactly-once unexpected-exit publication.
- `just test-firecracker` with the local v0.3 signed assets and Firecracker 1.16.0 — passed in
  33.453 seconds.
- `just lint` — passed with zero issues in both Go modules.
- Rust helper unit/process tests — 11 library tests and 2 inherited-descriptor process tests
  passed as part of `just test-microsandbox-linux`.
