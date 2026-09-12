---
title: Fast, Automated Qualification and Release
date: 2026-09-12
status: in-progress
owner: SecondStack
provenance: A full day of hand-driven qualification of PR #126 on the release host, 2026-09-12
---

# Plan: Fast, Automated Qualification and Release

## Outcome

One command qualifies a commit on this host in about 15 minutes with no
manual step, and one command takes a tagged commit to a staged release in
about 30 minutes. Both run every suite the release process requires today and
produce the same evidence documents; neither weakens a gate.

```sh
just qualify                 # PR tier: gates + sharded Firecracker suite, ~6 min
just qualify --tier release  # gates ∥ Firecracker ∥ gVisor host+pod (in the VM), ~15 min
just release 0.11.0          # qualify ∥ build → candidate → 3 parallel installer guests → stage
```

## What today costs and why

Measured on 2026-09-12 (32-thread, 125 GiB, KVM host; the no-KVM QEMU VM on
the same host):

| Stage | Wall clock | Driven by |
|---|---|---|
| Ten non-KVM gates | ~3 min | `just` recipes, by hand |
| Firecracker scenario suite (29 groups) | 6 to 7 min | `just test-scenario`, by hand |
| gVisor host + pod suites | 7 + 7 min | git bundle into the VM, rebuild guest agent, run, copy evidence, all by hand |
| Candidate staging | 5 min | by hand, env recovered from a previous release directory |
| Installer qualification | 21 to 25 min | three libvirt guests in series |

About 55 minutes serial when everything passes. Today it took six hours:

- Evidence is commit-exact for every suite, so each one-file fix (five today)
  invalidated all of it. Correct for the release commit; ruinous as the
  default for every push.
- `TestScenarioAttributedConnectionLossRevokesExecution/secondbox-runner`
  SIGKILLs the runner container and `compose start`s it; about one run in four
  the container never comes back, and every later test then waits 90 s for a
  runner that is dead until the 30-minute `go test` timeout. Three times today.
- Manual glue: VM shipping, evidence copying, the release env, the protoc
  pin, the `just` shim resetting Go, a local tag to create, move, and delete.
- Serial ordering where only the installer truly depends on the candidate.

## Fixed architecture

- No gate is removed or weakened. Release evidence stays commit-exact and
  keeps its schemas; gVisor evidence still comes from a host without KVM
  (the QEMU VM), which the automation drives instead of a person.
- Everything runs on this host from one checked-in example configuration
  copied to `~/.config/secondbox/qualify.env`; no script reads a path from
  a previous release's directory.
- Scenario tests keep their harness and semantics; sharding only changes
  which tests one `go test` invocation runs.
- The installer driver keeps its three modes and assertions; only their
  scheduling changes.

## Task 1: Reliable scenario harness

- [x] After the deliberate runner SIGKILL in `tests/scenario/attributed_execution_test.go`
  (and any other kill/start pair), bring the container back with
  `compose up -d --no-deps <service>` and poll `docker inspect` until it is
  running, retrying the start once; fail the test within 30 s with the compose
  and inspect output if it does not come back. Investigate why `compose start`
  after `kill -s SIGKILL` under `restart: unless-stopped` intermittently does
  nothing and fix the cause if it is in the harness or compose file.
- [x] Fail fast on a lost runner: when `waitForScenarioRunner` times out, record
  a package-level "runner lost since <test>" marker so every later fixture
  fails immediately with that reason instead of waiting 90 s each. Keep the
  first failure's diagnostics complete.
- [x] Capture each `scenarioCompose` invocation's output into the test log on
  failure, so a stalled restart is diagnosable after teardown.
- [x] Add `SECONDBOX_SCENARIO_SHARD=i/N` to `scripts/test-scenario.sh`: list the
  package's top-level tests with `go test -list`, split deterministically, and
  run only shard `i`. Sharded runs never write qualification evidence. The
  smoke template publish runs once per shard (it is the suite's precondition).
- [x] Cover the shard splitter with a test and prove two concurrent shards on
  this host complete without interfering (distinct project names and /24s).

Task 1 verification on the KVM release host (2026-09-12): the unsharded
Firecracker suite passed in 542 s; concurrent shards 1/2 and 2/2 passed in
451 s and 229 s (14 + 13 disjoint top-level tests). Each shard published one
template and used its own project and guest/Compose /24s. A deliberate no-op
start injection exercised both start attempts and the bounded failure; the
next fixture failed in 0.00 s with the original runner-lost marker. Generated
verification, Go tests, lint, scenario vet/compile, shell syntax, and diff
checks passed. No scenario assertions changed.

## Task 2: `just qualify`

- [x] `scripts/qualify.sh [--tier pr|release] [--only gates|firecracker|gvisor]`
  reading `~/.config/secondbox/qualify.env`, with `deploy/qualify.env.example`
  checked in and every key documented: microVM artifacts dir, artifact public
  key and fingerprint, workspace root, runtime and toolchain digests, gVisor VM
  directory (qcow2, seed, key), VM SSH port, VM build root and reflink mount.
  Missing or invalid values fail before anything starts.
- [x] PR tier runs, concurrently: the ten non-KVM gates (one process each),
  and the Firecracker suite sharded across N stacks (N from
  `QUALIFY_FIRECRACKER_SHARDS`, default 4). Target: under 6 minutes on this
  host.
- [ ] Release tier runs, concurrently: the gates, the unsharded Firecracker
  suite writing `.tmp/scenario-qualification-evidence.json`, and the gVisor
  chain in the VM: boot the QEMU VM if its SSH port is closed (the boot
  command from `docs/operations/gvisor-runtime.md`, CPU `host,-vmx,-svm`),
  wait for SSH, ship `HEAD` with `git bundle`, check it out detached, rebuild
  `secondbox-guest-agent` into the build root, remount the reflink volume if
  needed, run the host and pod suites as root under `systemd-run`, copy both
  evidence files back into `.tmp`, and leave the VM running. Target: under
  16 minutes on this host.
- [x] Every stage streams to its own log under `.tmp/qualify/<run>/` and the
  command ends with a table of stage, result, and wall clock; the exit status
  is non-zero if any stage failed. Stages run detached from the calling shell
  (`systemd-run --user` when available, `setsid` otherwise) so a dropped
  terminal does not kill them; `just qualify --wait <run>` reattaches.
- [x] Toolchain footguns: `scripts/verify-generated.sh` installs the pinned
  protoc into `.tmp/protoc` when the system version differs and uses it;
  `Justfile` sets `GOTOOLCHAIN` from `go.mod` so shims cannot change the Go
  version; `just qualify` refuses a dirty tree in release tier.
- [ ] Prove it: run `just qualify --tier pr` and `just qualify --tier release`
  on this host and record both timing tables in the plan.

### Task 2 validation

The ten gates are `verify-generated`, `test`, `test-contract`, `test-compose`,
`test-image-policy`, `test-sdk-packages`, `test-deployment`, `test-install-docs`,
`test-release-workflow`, and `lint`. Packaging waits for generated SDK output;
the processes otherwise start concurrently. Lint uses a cache per checkout:
sharing cached source paths across worktrees caused existing path exclusions to
miss. No lint rule or test assertion was changed.

The independent Task 2 checkout passed PR qualification with one stack in
8m03s (`20260912T215226-2424972`, source `8aabe8f`); its harness lacks Task 1.
The intended four-shard PR tier passed in **5m46s** in an isolated checkout
combining Task 1 with Task 2 (`20260912T220613-2997563`, source
`c3720a4443abe6dde1487cac83712a9099bd7761`). Task 2's branch does not include
the Task 1 dependency.

| Stage | Result | Wall clock |
|---|---|---|
| firecracker-1 | PASS | 2m 50s |
| firecracker-2 | PASS | 2m 33s |
| firecracker-3 | PASS | 5m 46s |
| firecracker-4 | PASS | 2m 36s |
| lint | PASS | 0m 1s |
| test-compose | PASS | 0m 30s |
| test-contract | PASS | 0m 4s |
| test-deployment | PASS | 0m 2s |
| test-image-policy | PASS | 0m 3s |
| test-install-docs | PASS | 0m 2s |
| test-release-workflow | PASS | 0m 0s |
| test-sdk-packages | PASS | 0m 11s |
| test | PASS | 2m 3s |
| verify-generated | PASS | 0m 6s |
| Total | PASS | 5m 46s |

Release attempt `20260912T215616-2591202` (assembled source `925e2a7`) did
**not** qualify. Firecracker passed, but the VM stage was stopped after another
operator's concurrent run was discovered. The lint failure below came from the
cross-worktree cache issue described above; the unchanged lint configuration
subsequently passed with an isolated cache.

| Stage | Result | Wall clock |
|---|---|---|
| firecracker | PASS | 8m 2s |
| gvisor | FAIL (1), interrupted | 2m 8s |
| lint | FAIL (1), cache paths | 0m 9s |
| test-compose | PASS | 0m 24s |
| test-contract | PASS | 0m 10s |
| test-deployment | PASS | 0m 6s |
| test-image-policy | PASS | 0m 5s |
| test-install-docs | PASS | 0m 3s |
| test-release-workflow | PASS | 0m 1s |
| test-sdk-packages | PASS | 0m 13s |
| test | PASS | 2m 37s |
| verify-generated | PASS | 0m 9s |
| Total | FAIL | 8m 2s |

Full release proof remains blocked on the no-KVM VM. After operator unit
`ux-gvisor-chain11` became inactive, its project `secondbox-suite-628122` still
had four containers, a network, and a volume. The final assembled source
`8ce10d194dee5c5074ac6418bf6ba68eaa499412` correctly refused that occupied VM
before starting any stage. Cleanup requires its owner or an explicit exception
to the instruction forbidding changes to resources created by others.

The driver now checks occupancy immediately before checkout and source identity
throughout the guest chain. Earlier independent release testing also reproduced
the known pre-Task-1 runner restart failure. The QEMU cold-boot path remains
untested because the existing VM is running and belongs to the operator.

## Task 3: `just release VERSION`

- [x] `scripts/release.sh VERSION` reading `~/.config/secondbox/release.env`
  (`deploy/release.env.example` checked in; a superset of qualify.env with the
  release source dir, release public key, Postgres image, candidate and
  installer directories, qualification image and digest).
- [x] Preconditions up front: clean tree, `HEAD` is on `main`, no existing
  `v<VERSION>` tag or one that already identifies `HEAD`, output directories
  absent, libvirt reachable with no `sbq-` domains, disk and memory headroom.
  Create the local tag when absent. Never push anything.
- [x] Run `qualify --tier release` and the artifact build concurrently; bind
  evidence into the candidate as soon as both finish
  (`release-stage.sh --candidate` already separates building from binding;
  split it if it does not).
- [x] Installer qualification with the three guests in parallel:
  `scripts/installer-qualification-driver` runs `run_guest` for each mode as a
  background job with its own SSH port and MAC, waits for all, and merges the
  three evidence files exactly as today. Guest memory is `QUALIFY_GUEST_MEMORY_MIB`
  (default 16384) and parallelism is capped by available host memory.
- [x] Final `release-stage`, then print the exact publish commands (tag push,
  upload) without running them. End with a timing table.
- [ ] Prove it end to end on this host against a throwaway version and record
  the timing table in the plan; delete the local tag afterwards.

## Task 4: Documentation

- [x] Rewrite the operator sequence in `docs/operations/release-operator-setup.md`
  and `docs/operations/scenario-qualification.md` around the two commands;
  keep the manual recipes as an appendix for hosts without the automation.
- [x] CHANGELOG entry under Unreleased.

## Deferred

- Self-hosted GitHub runners for the KVM host and the VM, so `just qualify`
  runs on every PR without a person.
- A compatibility test that runs the guest protocol against the guest agent
  binary inside the shipped bundle, which would have caught #128 without KVM.
