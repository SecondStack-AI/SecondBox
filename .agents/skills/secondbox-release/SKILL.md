---
name: secondbox-release
description: Cut, tag, publish, verify, or retract SecondBox releases; prepare release notes or rotate release bundles and trust anchors. Use for any v-prefixed release tag push in this repository.
---

# SecondBox release

Read [release operator setup](../../../docs/operations/release-operator-setup.md)
for mechanics and [worklog](../worklog/SKILL.md) for continuity. This skill owns
release decisions, preflight, recovery and close-out. Follow the user's existing
authorization for PRs, merges and publication; loading this skill does not grant
it. Prepare a reviewable result before requesting any missing authorization.

## Invariants

- Cut only with `just release VERSION` (or `--resume` after a gate-only
  failure) on the release host, from clean `main` equal to fetched
  `origin/main`. Never construct a release with `git tag`, `release-stage`
  and `release-upload`, or tag a PR branch. The manual operator appendix
  explains mechanics for exceptional hosts, not an alternative skill workflow.
- Push only after successful staging, with the printed command. Pushed tags
  are immutable. Retract a bad Go release in `go.mod`, document it in the
  changelog and release notes, then issue the next patch; never move the tag.
  Leave v0.13.0 intact: its off-main tag has the re-landed tree and downstream pins.
- Reserve the host for one release. Inspect `secondbox-suite-*` user units,
  `sbq-*` libvirt domains, matching containers, `.tmp/release/checkout.lock`, and
  `RELEASE_OUTPUT_ROOT/VERSION{,-build,-candidate}`. Identify owners and wait for
  live work; never stop another operator's resources.
- Freeze the checkout through final staging: no edits, branch switches or
  untracked notes. Prefer an operator-designated dedicated release worktree
  on `main`; creating/reassigning one is a separate setup choice. Worklogs go
  to the ignored primary-checkout ledger; other notes go outside the frozen tree.
- Launch long work using the operator doc's `systemd-run --user` command with a
  unique `secondbox-suite-release-VERSION-TIMESTAMP` unit. Check release status
  no more often than every five minutes (use a monitor when available). Keep
  the user informed without repeatedly polling. Do not rely on shell background jobs.
- Never publish incomplete artifacts, qualification waivers or source-only
  releases. Report “not released” with the failing stage when prerequisites fail.
- Land notes and the changelog cut on `main` before tagging, including pre-1.0.
  Only an explicit operator decision may skip them; record it in the ledger.
- Keep private signing keys and password-manager contents out of repository,
  notes, logs and worklog. Reference the key and item by name only.

## Procedure

1. Read the latest two worklog files and operator doc. Fetch `origin`, inspect
   `gh release list --limit 5` (local tags include unpublished/retracted tags),
   and `git log --oneline vLAST..origin/main`.
2. Choose a minor version pre-1.0 for protocol generation, migration checksum,
   bundle/trust-anchor or clean-install boundary changes; patch otherwise.
   Ask only if the evidence leaves a consequential ambiguity.
3. Preflight without changing host state: check ownership above; compare the
   **key names** in `release.env` with `deploy/release.env.example` without
   printing secrets; verify the public anchor's DER SHA-256 against the notes;
   inspect `df` (200 GiB free per output/workspace filesystem) and MemAvailable
   (guest plus 16 GiB). Check dependencies match the lockfile; provision with
   `npm ci --ignore-scripts` before launch if needed. For `--full`, reserve the
   no-KVM VM and verify the reviewed arm64 binfmt registration and Buildx platforms.
4. Prepare branch `release/vX.Y.Z`, PR title `docs(release): prepare vX.Y.Z`:
   cut `## Unreleased` to `## X.Y.Z - DATE` and leave a fresh Unreleased section;
   write `docs/releases/vX.Y.Z.md` using v0.14.0 as the structure (deployment
   boundary, bundle identity, changes by area, distribution). Update boundary
   docs and verify the version-neutral downstream integration instructions.
   Wait for CI and follow the repository/operator review and merge policy;
   do not assume notes-only PRs waive a required review.
5. After merge, `git switch main` and `git pull --ff-only`; verify HEAD equals
   `origin/main` and contains the notes merge. Launch `just release X.Y.Z`
   under the documented user service. Record HEAD, UTC start, unit and run ID
   in the worklog. Wait for the final result and inspect `timing.md` and failed logs.
6. On failure, use the table below. When only a gate failed and the build and
   every scenario stage passed, relaunch as `just release X.Y.Z --resume` (add
   `--full` if the original run had it): it verifies the retained build and
   commit-exact evidence, requalifies the gates, and continues from candidate.
   Any other failure means a full relaunch; remove only this run's candidate
   and final directories first. A code change must land on main first and
   invalidates all commit-bound evidence. Delete a local tag only after checking
   the remote has no such tag; then relaunch the entire flow at the new merged commit.
7. After successful staging, execute the printed tag push and then
   `just release-upload X.Y.Z OUTPUT` in order within publication authorization.
   Upload reads `docs/releases/vX.Y.Z.md` from the tag automatically, or accepts
   an explicit third `NOTES_FILE` argument. Do not edit the published body as
   routine close-out. Locate the dispatched `release.yml` run for this version
   (do not assume the newest run is yours) and watch its ID with `--exit-status`.
8. Verify `gh release view vX.Y.Z --json isDraft,isPrerelease,body,assets`: stable,
   expected notes plus install/SDK footer, complete assets matching the staged
   manifest. Historical counts were 29 lean / 30 full; derive expectations from
   this release instead of treating those counts as permanent. Check
   `npm view @secondstack-ai/secondbox@X.Y.Z version`, and use a fresh temporary
   `GOMODCACHE` with `GOFLAGS=-mod=mod GOPROXY=https://proxy.golang.org go list -m
   github.com/SecondStack-AI/SecondBox@vX.Y.Z`. Inspect all six GHCR version tags
   with `docker buildx imagetools inspect` (control-plane, runner, installer-tools,
   microvm-artifacts, runner-gvisor, gvisor-artifacts). Verify the latest
   downloaded `install.sh` embeds the new version; do not execute it to check.
9. Append timings, coordinates and lessons to the worklog; Claude also updates
   memory. Supply the [downstream hand-off](../../../docs/operations/downstream-release-integration.md):
   manifest URL/digest, SHA256SUMS digest, npm integrity, OCI and binary digests,
   Profile revision/spec digests, platform matrix and protocol window. State
   whether deployments may update or must reinstall. After success remove this
   run's build/candidate directories and temporary worktrees it created, within
   existing cleanup authorization; retain final artifacts and evidence.

## Failure triage

| Symptom | Investigation and action |
| --- | --- |
| Output already exists | Confirm this failed run owns it; archive or remove only its versioned outputs before a full retry. Retain the build for recovery. |
| Checkout lock held | Inspect owning processes and units; wait for a live owner. `flock` releases on process exit. A leftover file is harmless: never unlink a held lock (that could permit concurrent runs). |
| `sbq-` domains | Confirm owner is gone before destroying/undefining this run's orphan with `virsh -c qemu:///system`; preserve other guests. |
| Missing env key or anchor mismatch | Compare example keys and independently verified public pins; correct operator config, not script validation. |
| Several tests fail at 0.00s | Inspect disposable Postgres readiness and environment leaks; `--resume` once. A repeat requires fixing the cause. |
| `UnsafeConfiguration/group_or_other_readable` | Correct unsafe file modes; never weaken the test. |
| Firecracker “Runner did not enroll” cascade | Inspect enrollment logs; retry once if evidence indicates the known restart flake. Repetition requires investigation. |
| Control-plane timeouts | Check host load and `QUALIFY_MAX_STACKS`; avoid concurrent suites. |
| Storage pressure admission denied | Check /tmp and workspace disk use; free only owned disposable data, never lower admission thresholds. |
| Firecracker websocket close 1006 | Compare shipped guest bundle provenance with guest commits; inspect the host firewall. |
| Full arm64 build fails | Verify binfmt after reboot and selected Buildx platforms; use reviewed host setup, not a guessed privileged installer. |
| Wrong Go / mise version absent | Load release.env and pin RELEASE_GOROOT as below. |
| Lint errors from another worktree | Use `GOLANGCI_LINT_CACHE=$PWD/.tmp/golangci-lint`. |
| npm 404 after publish | Poll with a five-minute bound, then inspect the matching workflow log. |
| Historical published body is an install one-liner | For an explicitly authorized repair, use `gh release edit vX.Y.Z --notes-file docs/releases/vX.Y.Z.md`; new publication preserves notes automatically. |

## Gates-only recovery from a retained build

`just release X.Y.Z --resume` is the only recovery path. It refuses to run
unless `RELEASE_OUTPUT_ROOT/X.Y.Z-build` exists with its checksums, every
required scenario evidence file names HEAD (pod evidence too for `--full`),
and the candidate and final directories are absent. It then requalifies the
gates, builds the candidate from the retained build, runs installer
qualification, and stages the final release, printing the same publish
commands as a fresh run. Launch it under the same user-service wrapper and
record the original and resumed run IDs in the worklog. If the preconditions
fail, do not assemble the stages by hand; relaunch the full flow.
