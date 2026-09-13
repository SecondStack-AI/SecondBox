---
title: Fast, Automated Qualification and Release
date: 2026-09-12
status: completed
owner: SecondStack
provenance: A full day of hand-driven qualification of PR #126 on the release host, 2026-09-12
---

# Plan: Fast, Automated Qualification and Release

## Outcome

Tasks 1–4 below record the original full-matrix pipeline. Task 5 supersedes
its tier selection, evidence placement, and installer scheduling defaults.

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
- [x] Release tier runs, concurrently: the gates, the unsharded Firecracker
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
- [x] Prove it: run `just qualify --tier pr` and `just qualify --tier release`
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

At Task 2 handoff, full release proof was blocked on the no-KVM VM. After operator unit
`ux-gvisor-chain11` became inactive, its project `secondbox-suite-628122` still
had four containers, a network, and a volume. The final assembled source
`8ce10d194dee5c5074ac6418bf6ba68eaa499412` correctly refused that occupied VM
before starting any stage. Cleanup required its owner or an explicit exception
to the instruction forbidding changes to resources created by others.

The driver now checks occupancy immediately before checkout and source identity
throughout the guest chain. Earlier independent release testing also reproduced
the known pre-Task-1 runner restart failure. The QEMU cold-boot path remains
untested because the existing VM is running and belongs to the operator.

### Completed release-tier proof

Task 3 completed the release-tier proof on 2026-09-12 (local host date), using
`/usr/bin/just qualify --tier release` as the qualification stage of
`just release 0.99.0`. Run `20260913T000100-2896357` qualified clean source
`e740d0a9b87ee86d7e7b9577789058b796d50e63` in an isolated clone on local `main`.
All ten gates, unsharded Firecracker, and gVisor host and pod suites passed.
The **16m13s** total is **13 seconds above the under-16-minute target**, while
artifact building ran concurrently. No gate or assertion was reduced. The
existing running no-KVM VM was used; its cold-boot path remains untested.

| Stage | Result | Wall clock |
|---|---|---|
| firecracker | PASS | 7m 43s |
| gvisor-host | PASS | 8m 4s |
| gvisor-pod | PASS | 8m 2s |
| gvisor | PASS | 16m 13s |
| lint | PASS | 0m 1s |
| test-compose | PASS | 0m 19s |
| test-contract | PASS | 0m 4s |
| test-deployment | PASS | 0m 3s |
| test-image-policy | PASS | 0m 3s |
| test-install-docs | PASS | 0m 1s |
| test-release-workflow | PASS | 0m 0s |
| test-sdk-packages | PASS | 0m 10s |
| test | PASS | 1m 36s |
| verify-generated | PASS | 0m 5s |
| Total | PASS | 16m 13s |

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
- [x] Prove it end to end on this host against a throwaway version and record
  the timing table in the plan; delete the local tag afterwards.

### Task 3 validation

`just release 0.99.0` passed end to end in **29m42s**, run
`20260913T000059-2895705`, on source
`e740d0a9b87ee86d7e7b9577789058b796d50e63`. The proof used an isolated clone
whose local `main` identified the task commit, leaving the task branch and the
original repository's `main` untouched. The local throwaway tag was deleted
afterward. Nothing was pushed or uploaded.

| Stage | Result | Wall clock |
|---|---|---|
| build | PASS | 5m 14s |
| candidate | PASS | 0m 6s |
| installer | PASS | 13m 14s |
| qualification | PASS | 16m 16s |
| stage | PASS | 0m 6s |
| Total | PASS | 29m 42s |

The installer driver selected three concurrent guests at its default 16384 MiB.
`virsh dominfo` confirmed all three running together with 8 vCPUs and
16777216 KiB each. Mode timings were 9m07s (`existing_reflink_filesystem`),
10m39s (`btrfs_image`), and 13m13s (`existing_reflink_recreation`). All 25 merged
assertions and reboot checks passed. All three domains and their workspace
files were removed; the unrelated `secondstack-preview-k8s` domain was left
alone. The final manifest and installer evidence bind the candidate's exact
qualification subject. Final files remain in
`/home/sasha/Developer/tries/secondbox-fq-release/releases/0.99.0`.

Additional validation passed: Bash syntax for all 81 Bash scripts under
`scripts`, `runner/scripts`, and `tests`; `just lint`; `just test-install-docs`;
`just test-deployment`; `just test-release-stage`; and `git diff --check`.
The release-stage suite proves build/bind byte equivalence, corruption and
extra-file rejection, and version binding. Scheduler coverage proves three-way
overlap, a one-guest cap, and waiting for surviving guests after one fails.

Implementation findings and deviations:

- `release-stage --candidate` required evidence before building, so staging was
  split with `--build-only` and `--from-build`. Candidate and final binding reuse
  a checksummed intermediate build; the ordinary manual staging path remains.
- A dedicated, pinned Buildx builder was provisioned to avoid modifying the
  host's existing builder container. The initial integration run accidentally
  applied that builder to scenario Docker builds, which did not load their
  runner image. That attempt correctly failed qualification in 14m30s while
  its other gates and both gVisor suites passed. `RELEASE_BUILDX_BUILDER` is now
  scoped to artifact staging only; the entire proof was rerun from the fix.
  The dedicated builder is stopped, with its configuration and cache retained.
- The release-stage regression suite exposed a pre-existing stale assertion:
  synthetic component rotation plus attributed execution produces four
  `agent-compartment` revisions, not three. The test now requires the exact
  four-revision lineage and attributed policy. No assertion was removed or
  generalized to accept multiple outcomes.

## Task 4: Documentation

- [x] Rewrite the operator sequence in `docs/operations/release-operator-setup.md`
  and `docs/operations/scenario-qualification.md` around the two commands;
  keep the manual recipes as an appendix for hosts without the automation.
- [x] CHANGELOG entry under Unreleased.

## Task 5: Lean release tier (pre-release)

Measured on 2026-09-13 for v0.11.0 on this host: build 7m26s, qualification
16m24s (Firecracker 8m20s, gVisor host 8m43s + pod 7m32s in the VM),
installer 14m56s, total 31m32s. The project is pre-release; the release
pipeline should prove what a user can hit today and nothing else. The full
matrix moves to a nightly tier and stays available on demand.

Decisions:

- **Release tier** = gates ∥ amd64-only artifact build ∥ Firecracker suite
  sharded 4-way ∥ gVisor host suite sharded 4-way on this host, then bind,
  then one installer guest (fresh Btrfs-image install through the wizard,
  reboot recovery, and the hello-world microVM), then stage. Target: about
  10 minutes end to end.
- **Nightly tier** = everything the release tier drops: linux/arm64 images,
  the customer-shared-tenancy and snapshot-resume scenarios (with the
  template publish smoke), the gVisor pod suite in the VM, and the two other
  installer guests (existing-filesystem uninstall/resume, and the v0.7.2
  refuse-and-recreate boundary). `just qualify --tier nightly` runs it;
  `just release --full VERSION` runs a release with it.
- gVisor is a supported backend, so its host suite stays in the release tier.
  It runs on this host, which has KVM; the evidence rule that gVisor evidence
  must come from a host without KVM is dropped, because runsc does not use
  KVM in this deployment and the rule only existed to force the VM. The VM
  remains the nightly pod-suite host.
- Sharded runs may produce release evidence: the shards' per-test results are
  merged into one evidence document (sum of passes, maximum wall clock, the
  same commit and dirty checks) with the existing schema, so `release-stage`
  needs no new evidence kind.
- Scenario tiers are selected in one place: a `SECONDBOX_SCENARIO_TIER`
  (`release` default, `nightly`) that the harness maps to a `-skip` list;
  no test is deleted. `TestScenarioTouchExtendsIdleExpiry` uses a 5-second
  idle Profile so it no longer sleeps 25 s.
- The installer driver takes a mode list; `vm-scenario.json` lists required
  assertions per mode so the release tier's single guest is judged on the
  assertions that guest can make.
- Artifact manifest and release verification accept an amd64-only image set
  when the release tier built it; the manifest records the platforms built.

- [x] Harness: `SECONDBOX_SCENARIO_TIER` and the skip list; the 5-second idle
  Profile for the touch test; shard evidence merge in `scripts/test-scenario.sh`
  (each shard writes `.tmp/scenario-shard-<i>-evidence.json`; the merge writes
  the ordinary evidence file and refuses shards from different commits).
- [x] `scripts/qualify.sh`: release tier runs Firecracker and gVisor host
  sharded on this host and merges evidence; `--tier nightly` adds the
  dropped scenarios unsharded, the pod suite in the VM, and keeps the
  no-KVM evidence for it. `scripts/qualify-gvisor.sh` gains a `--host` mode
  that runs the gVisor suite locally with the same build root inputs the VM
  uses (`QUALIFY_GVISOR_HOST_BUILD_ROOT` in the env example).
- [x] `scripts/release-stage.sh`: accept gVisor evidence from a KVM host;
  `RELEASE_IMAGE_PLATFORMS` (default `linux/amd64` in the lean tier,
  `linux/amd64,linux/arm64` with `--full`); the manifest records the platform
  list and `pkg/releaseverify` accepts it; cross-compiled CLI and deploy
  binaries stay for all four host platforms (they are seconds).
- [x] `scripts/installer-qualification-driver`: `--modes` list; per-mode
  required assertions in `tests/installer/vm-scenario.json`;
  `scripts/test-installer-qualified.sh` passes the release tier's single mode
  by default and all three with `--full`.
- [x] `scripts/release.sh`: `--full` flag; default lean. `Justfile`: `release
  version *flags`, `qualify` unchanged, a `nightly` recipe that runs
  `qualify --tier nightly` and is safe to put on a systemd timer.
- [x] Docs: `docs/operations/release-operator-setup.md` and
  `scenario-qualification.md` describe the two tiers; CHANGELOG entry.
- [x] Prove on this host: `just qualify` (PR tier), `just qualify --tier
  release`, `just release 0.99.0` (lean) with timing tables in this plan;
  delete the throwaway tag. Run the nightly tier once to prove it still
  passes end to end and record its table too.

### Task 5 qualification proof — 2026-09-13

The first four successful runs below used clean source commit
`f095a352867d154f00748c8818b5e27dd95fe5ff` on this KVM host.
The release proof used an isolated local clone and private copies of the
operator environment files, a dedicated Buildx builder, and separate installer
workspace paths. The original operator files and production stacks were not
modified. Releases were staged locally; nothing was pushed or uploaded.

#### PR qualification: `just qualify`

Run `20260913T135334-1129191`.

| Stage | Result | Wall clock |
|---|---|---|
| firecracker-1 | PASS | 2m 12s |
| firecracker-2 | PASS | 1m 40s |
| firecracker-3 | PASS | 3m 13s |
| firecracker-4 | PASS | 2m 8s |
| firecracker | PASS | 3m 13s |
| lint | PASS | 0m 2s |
| test-compose | PASS | 0m 29s |
| test-contract | PASS | 0m 4s |
| test-deployment | PASS | 0m 4s |
| test-image-policy | PASS | 0m 3s |
| test-install-docs | PASS | 0m 1s |
| test-release-workflow | PASS | 0m 0s |
| test-sdk-packages | PASS | 0m 10s |
| test | PASS | 3m 16s |
| verify-generated | PASS | 0m 6s |
| Total | PASS | 3m 39s |

#### Release qualification: `just qualify --tier release`

Run `20260913T133028-53235`.

| Stage | Result | Wall clock |
|---|---|---|
| firecracker-1 | PASS | 1m 57s |
| firecracker-2 | PASS | 1m 25s |
| firecracker-3 | PASS | 2m 53s |
| firecracker-4 | PASS | 1m 46s |
| firecracker | PASS | 2m 53s |
| gvisor-1 | PASS | 1m 29s |
| gvisor-2 | PASS | 2m 11s |
| gvisor-3 | PASS | 2m 56s |
| gvisor-4 | PASS | 1m 43s |
| gvisor-host | PASS | 2m 57s |
| lint | PASS | 0m 2s |
| test-compose | PASS | 0m 22s |
| test-contract | PASS | 0m 4s |
| test-deployment | PASS | 0m 4s |
| test-image-policy | PASS | 0m 4s |
| test-install-docs | PASS | 0m 2s |
| test-release-workflow | PASS | 0m 1s |
| test-sdk-packages | PASS | 0m 11s |
| test | PASS | 1m 46s |
| verify-generated | PASS | 0m 7s |
| Total | PASS | 3m 6s |

#### Lean release: `just release 0.99.0`

Run `20260913T133338-243118`.

| Stage | Result | Wall clock |
|---|---|---|
| build | PASS | 5m 4s |
| candidate | PASS | 0m 6s |
| installer | PASS | 4m 20s |
| qualification | PASS | 3m 44s |
| stage | PASS | 0m 5s |
| Total | PASS | 9m 35s |

#### Nightly qualification (inside `just release 0.99.1 --full`)

Run `20260913T134352-698392`.

| Stage | Result | Wall clock |
|---|---|---|
| firecracker | PASS | 8m 20s |
| gvisor-host | PASS | 7m 50s |
| gvisor-pod | PASS | 9m 24s |
| gvisor | PASS | 9m 30s |
| lint | PASS | 0m 2s |
| test-compose | PASS | 0m 16s |
| test-contract | PASS | 0m 4s |
| test-deployment | PASS | 0m 4s |
| test-image-policy | PASS | 0m 3s |
| test-install-docs | PASS | 0m 1s |
| test-release-workflow | PASS | 0m 0s |
| test-sdk-packages | PASS | 0m 11s |
| test | PASS | 1m 41s |
| verify-generated | PASS | 0m 6s |
| Total | PASS | 9m 41s |

The lean release built amd64 control-plane images and all four CLI/deploy
host platforms. Its single Btrfs-image guest passed all 11 fresh-install,
reboot, and hello-world assertions. The throwaway `v0.99.0` tag was deleted.
The merged release evidence recorded 27 Firecracker and 25 gVisor passes;
both documents retain the existing v2 schema, clean source identity, summed
passes, and maximum shard duration. Local gVisor evidence records KVM present;
nightly pod evidence retains the no-KVM requirement.

Validation passed: `bash -n` on all 92 repository shell scripts;
`just verify-generated`, `just lint`, `just test`, `just test-deployment`,
`just test-release-workflow`, `just test-install-docs`, `just test-release-stage`,
`scripts/test-qualify.sh`, and
`go test ./pkg/releaseverify ./cmd/secondbox-release-tool ./pkg/releasecontract`.
The real qualification runs also passed Compose, contract, image-policy, and
SDK gates. Targeted deployment regressions cover invalid shard merges,
installer mode scheduling, and local gVisor resource isolation.

#### Reboot-readiness proof and interrupted full-release continuations

The reboot-readiness installer implementation is
`97e2f123a173e500ba69f4ba53fea29ff2f277bc`. Its root-filesystem nightly run
`20260913T141853-2092623` passed with the following table:

| Stage | Result | Wall clock |
|---|---|---|
| firecracker | PASS | 8m 2s |
| gvisor-host | PASS | 6m 42s |
| gvisor-pod | PASS | 8m 55s |
| gvisor | PASS | 9m 2s |
| lint | PASS | 0m 16s |
| test-compose | PASS | 0m 20s |
| test-contract | PASS | 0m 11s |
| test-deployment | PASS | 0m 7s |
| test-image-policy | PASS | 0m 5s |
| test-install-docs | PASS | 0m 2s |
| test-release-workflow | PASS | 0m 0s |
| test-sdk-packages | PASS | 0m 15s |
| test | PASS | 1m 43s |
| verify-generated | PASS | 0m 9s |
| Total | PASS | 9m 16s |

The corresponding `just release 0.99.3 --full` run
`20260913T141852-2091779` passed build (5m54s), qualification (9m18s), and
candidate binding (6s), but guest launch failed in 3s because the newly created
proof parent directories lacked libvirt traversal permission. This was proof
setup, not a guest assertion failure. After granting `libvirt-qemu` traversal
on those two task-owned directories, manual continuations reran
`just test-installer-qualified --full` against the same clean source, candidate,
and immutable build. Their later failures are recorded below. Following the
registry-switch fixture fix, a fresh full release regenerated artifacts and
evidence for the new source commit; no failed run is labeled PASS.

#### Final-source full-release proof

Source `299376d22dc3f58c3ff1b5d4ded87a83b49094bd`, clean isolated checkout
on root Btrfs. `just release 0.99.4 --full` uses 32 GiB installer guests;
actual available-memory scheduling selects one guest at a time.

Nightly qualification run `20260913T150010-3576631`:

| Stage | Result | Wall clock |
|---|---|---|
| firecracker | PASS | 7m 43s |
| gvisor-host | PASS | 6m 30s |
| gvisor-pod | PASS | 8m 41s |
| gvisor | PASS | 8m 48s |
| lint | PASS | 0m 2s |
| test-compose | PASS | 0m 15s |
| test-contract | PASS | 0m 4s |
| test-deployment | PASS | 0m 3s |
| test-image-policy | PASS | 0m 3s |
| test-install-docs | PASS | 0m 1s |
| test-release-workflow | PASS | 0m 0s |
| test-sdk-packages | PASS | 0m 10s |
| test | PASS | 1m 27s |
| verify-generated | PASS | 0m 5s |
| Total | PASS | 8m 58s |

Full release run `20260913T150010-3575009`:

| Stage | Result | Wall clock |
|---|---|---|
| build | PASS | 5m 38s |
| candidate | PASS | 0m 6s |
| installer | PASS | 15m 49s |
| qualification | PASS | 8m 59s |
| stage | PASS | 0m 6s |
| Total | PASS | 25m 0s |

All three installer modes passed: Btrfs image 3m43s, existing filesystem
4m51s, and v0.7.2 refusal/recreation 7m14s. Final installer evidence records
25 passes, reboot recovery, and the exact clean source commit. The final
manifest records both control-plane platforms, four host-binary platforms,
and the no-KVM pod evidence reference. The complete `just release --full`
command passed; `v0.99.4` was deleted and nothing was pushed or uploaded.

Final artifacts are retained at `/var/tmp/secondbox-task5-proof/releases/0.99.4`.
All disposable installer guests and their 19 task-owned directory-pool records
were cleaned up. The dedicated Buildx builder is stopped with its cache retained.
The final proof-record commit changes only this plan; executable source was
qualified at `299376d22dc3f58c3ff1b5d4ded87a83b49094bd`.

Implementation findings and deviations:

- Concurrent local gVisor suites exposed previously shared network profile
  slots. Each suite now locks its own pair, avoiding profiles declared by
  existing containers. The supported local shard maximum is seven; four is
  the default. Backend-specific shard directories prevent evidence collisions.
- Docker's privileged-device snapshot omitted loop nodes created after the
  container started. The gVisor scenario runners now bind host `/dev`, with
  the existing direct runner PID 1 and no console/seat services.
- Host UFW blocked marked gVisor DNS and attribution traffic. Scenario setup
  temporarily permits only the suite's reserved interfaces and backend mark;
  cleanup removes only rules carrying that exact suite owner. Production
  containers, networks, volumes, and services were left untouched.
- Moving template publication to nightly required capability expectations to
  follow the explicit snapshot-resume template input. All scenarios remain in
  the tree; tiers select them centrally.
- The lean Btrfs guest completes a fresh wizard installation before reboot.
  Interruption, uninstall/resume, purge, and refuse/recreate coverage remains
  in the full installer modes. Evidence schemas remain unchanged.
- `RELEASE_ENV_FILE` enables isolated proof inputs without modifying the
  operator's working configuration. Artifact builds used a dedicated builder;
  its retained cache makes subsequent timing comparisons dependent on cache
  state. The final PR proof overlapped the full installer guests.
- The first full-release attempt (`20260913T134351-697268`) passed nightly
  qualification and the Btrfs/existing-filesystem guests, but recreation raced
  the control-plane restart after reboot. Its first login received a connection
  reset; command substitution then continued into misleading credential errors.
  The existing runner-readiness wait now precedes workload login, and Bash
  command substitutions inherit `errexit`. The already-failed guest SSH command
  was terminated so the driver could clean up; its tag was deleted. The next full
  proof used `97e2f123a173e500ba69f4ba53fea29ff2f277bc`.
- The next attempt (`20260913T140656-1593573`) passed the build and both
  gVisor suites but hit Firecracker's 90% storage admission threshold on the
  development filesystem during artifact building. The failed scenario was
  terminated and cleaned up; `v0.99.2` was deleted. No threshold was relaxed.
  Subsequent full proof uses `/var/tmp/secondbox-task5-proof`: an isolated
  checkout, copied signed immutable assets, and its own scenario/installer
  workspaces and release outputs on the root Btrfs filesystem. Earlier local
  artifacts were moved into its `prior-releases` directory. Original operator
  inputs and unrelated storage remain untouched.
- During the first installer continuation the host was no longer idle:
  approximately 106 GiB RAM and 57 GiB swap were in use, with load average 33.
  The existing-filesystem guest timed out waiting for verified materialization.
  Its sibling guests were terminated and all three cleaned up. A retry with
  `QUALIFY_GUEST_MEMORY_MIB=8192` was accepted by the driver but rejected by
  the installer’s 12 GiB host preflight. The next continuation uses 32 GiB
  guests, which available-memory scheduling runs one at a time. No timeout,
  preflight, or assertion changed, and unrelated workloads were not stopped.
- The serial continuation passed Btrfs (3m41s) and existing-filesystem (5m08s),
  then recreation failed while switching from real GHCR to its local candidate
  registry. Skopeo verified the local digest, but Docker returned “not found”
  for that same digest. This points to cached daemon registry routing after
  the hosts-file switch. The disposable guest now restarts Docker at that
  switch, before loading/pulling candidate images. No host Docker service is
  restarted. The passing full release from `299376d` regenerated all build and
  qualification evidence after this fixture fix.
- Initial failing attempts exposed the capability, network profile, loop-device,
  and firewall issues above; their timings are not counted as successful proof.
  The gVisor VM was already running, so this proves its nightly pod path but
  does not re-prove cold boot. An unrelated concurrent scenario project was
  observed and left untouched.

## Deferred

- Self-hosted GitHub runners for the KVM host and the VM, so `just qualify`
  runs on every PR without a person.
- A compatibility test that runs the guest protocol against the guest agent
  binary inside the shipped bundle, which would have caught #128 without KVM.

## Task 5 hardening: shared stack budget

The first real lean release overloaded this 32-thread host with eight scenario
stacks plus the ten gates. `firecracker-1` lost HTTP access to its control plane
for four minutes. The previous 3m06s qualification was insufficient reliability
proof for that schedule.

- `QUALIFY_MAX_STACKS` defaults to four and bounds scenario processes through
  teardown across both backends. The ten gates start first, and `test` reserves
  one slot until its status is published. A budget of one runs test before stacks.
- Admission is serialized until the preceding stack's published HTTP listener
  answers `/readyz`; Compose health alone does not release admission. A failed
  startup releases admission after teardown, retaining its failing stage status.
- `QUALIFY_GATES_FIRST=1` permits an explicit all-gates-before-stacks comparison.
  The default overlaps gates with three stacks, then permits four stacks.
- The HTTP client captures control-plane and runner container logs at the first
  request timeout per fixture, before later polling or cleanup. The original
  response/error and test assertions are preserved. The existing failure
  teardown still captures application logs and container state.
- Network selection uses PID-based candidates and persistent lock-file inodes,
  not second-resolution timestamps. Both guest and Compose subnets use the same
  lock namespace under the explicit shared workspace root, held through teardown.
  Eight simultaneous selectors starting at the same candidate reserved 16
  distinct subnets; reuse succeeded only after owner exit. The existing gVisor
  pair test also confirms exclusion of occupied and concurrently reserved pairs.

Proof runs must wait for `systemctl --user list-units 'sbx-dxd-*' --state=active`
(with no active unit rows) and no `secondbox-suite-*` containers, including stopped
containers. Proof configuration uses separate disposable workspaces and release
outputs on root Btrfs: the Developer filesystem is already at 90% usage. Original
operator configuration, other checkouts, and unrelated host resources are preserved.
