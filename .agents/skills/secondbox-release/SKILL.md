---
name: secondbox-release
description: Cut, tag, publish, verify, or retract SecondBox releases; prepare release notes or rotate release bundles. Use for any v-prefixed release tag push in this repository.
---

# SecondBox release

Read [release operator setup](../../../docs/operations/release-operator-setup.md)
for mechanics and runner setup, and [worklog](../worklog/SKILL.md) for continuity. This skill owns
release decisions, preflight, recovery and close-out. Follow the user's existing
authorization for PRs, merges and publication; loading this skill does not grant
it. Prepare a reviewable result before requesting any missing authorization.

## Invariants

- A release is cut only by pushing a `vX.Y.Z` tag on a merged commit of
  `origin/main`; `release-tag.yml` qualifies and stages it on the self-hosted
  release runners and dispatches the hosted publisher. Never tag a PR branch,
  run `release-upload` by hand, or construct a release from separate
  `release-stage` and upload commands.
- Push the tag only after the notes and the changelog cut are on `main`,
  including pre-1.0. Only an explicit operator decision may skip them; record
  it in the ledger.
- A pushed tag is immutable, even when its release failed: the Go module proxy
  can cache it at once. A fix ships as the next patch; retract the failed or bad
  version in `go.mod`, document it in the changelog and release notes; never
  move or re-push a tag. Only `-dryrun.N` tags are deleted after use.
  Leave v0.13.0 intact: its off-main tag has the re-landed tree and downstream pins.
- The release runners are dedicated hosts. Do not stop another operator's
  units, domains, Compose stacks, or live deployment on them; take paths from
  each runner's `release.env`, never assume them.
- Never publish incomplete artifacts, qualification waivers or source-only
  releases. Report "not released" with the failing job and stage.
- Keep credentials and password-manager contents out of repository, notes,
  logs and worklog. Reference each item by name only.

## Procedure

1. Read the latest two worklog files and the operator doc. Fetch `origin`,
   inspect `gh release list --limit 5` (local tags include unpublished/retracted
   tags), and `git log --oneline vLAST..origin/main`.
2. Choose a minor version pre-1.0 for protocol generation, migration checksum,
   bundle or clean-install boundary changes; patch otherwise.
   Ask only if the evidence leaves a consequential ambiguity.
3. Check both release runners are online and idle (`gh api
   repos/SecondStack-AI/SecondBox/actions/runners` needs admin; otherwise
   check a recent run or the hosts). For a changed release input (bundle,
   installer image, Postgres pin), update each runner's `release.env` first and
   compare its **key names** with the matching example without printing secrets.
4. Prepare branch `release/vX.Y.Z`, PR title `docs(release): prepare vX.Y.Z`:
   cut `## Unreleased` to `## X.Y.Z - DATE` and leave a fresh Unreleased section;
   write `docs/releases/vX.Y.Z.md` using v0.14.0 as the structure (deployment
   boundary, bundle identity, changes by area, distribution). Update boundary
   docs and verify the version-neutral downstream integration instructions.
   Wait for CI and follow the repository/operator review and merge policy;
   do not assume notes-only PRs waive a required review.
5. After merge, fetch, tag the merge commit (`git tag vX.Y.Z origin/main`
   after confirming it contains the notes merge) and push the tag within
   publication authorization. Record the commit, UTC start and the
   `release-tag.yml` run ID in the worklog. Watch it with `gh run watch RUN
   --exit-status`, checking no more often than every five minutes.
6. On failure, read the failed job log and the stage logs kept on that runner
   under `RELEASE_OUTPUT_ROOT/logs/`; use the table below. A flake or host
   problem is recovered with **Re-run failed jobs**: each job replaces this
   version's earlier outputs on its host. A code change lands on main first and
   invalidates all commit-bound evidence; it ships as the next patch version.
7. The `upload-amd64` job dispatches `release.yml`. Locate that run for this
   version (do not assume the newest run is yours) and watch it with
   `--exit-status`. Do not edit the published body as routine close-out.
8. Verify `gh release view vX.Y.Z --json isDraft,isPrerelease,body,assets`: stable,
   expected notes plus fenced install and SDK footer, complete assets matching the staged
   manifests. Derive asset expectations from this release instead of treating
   historical counts as permanent. Check
   `npm view @secondstack-ai/secondbox@X.Y.Z version`, and use a fresh temporary
   `GOMODCACHE` with `GOFLAGS=-mod=mod GOPROXY=https://proxy.golang.org go list -m
   github.com/SecondStack-AI/SecondBox@vX.Y.Z`. Inspect all six GHCR version tags
   with `docker buildx imagetools inspect` (control-plane, runner, installer-tools,
   microvm-artifacts, runner-gvisor, gvisor-artifacts) and the four `vX.Y.Z-arm64`
   tags (control-plane, runner, installer-tools, microvm-artifacts). Verify the latest
   downloaded `install.sh` embeds the new version; do not execute it to check.
9. Append timings, coordinates and lessons to the worklog; Claude also updates
   memory. Supply the [downstream hand-off](../../../docs/operations/downstream-release-integration.md):
   both manifest URLs/digests, SHA256SUMS digests, npm integrity, OCI and binary digests,
   Profile revision/spec digests, platform matrix and protocol window. State
   whether deployments may update or must reinstall. Final outputs stay on the
   runners under `RELEASE_OUTPUT_ROOT`; prune old versions when disk runs short.

## Failure triage

| Symptom | Investigation and action |
| --- | --- |
| `sbq-` domains | A cancelled job can leave its installer guest. Confirm no release job runs, then destroy/undefine that orphan with `virsh -c qemu:///system`. |
| Job queued, never starts | The runner for that label is offline or busy; check its service on the host. |
| Missing env key | Compare with the example keys; correct operator config, not script validation. |
| Several tests fail at 0.00s | Inspect disposable Postgres readiness and environment leaks; re-run the failed job once. A repeat requires fixing the cause. |
| `UnsafeConfiguration/group_or_other_readable` | Correct unsafe file modes; never weaken the test. |
| Firecracker “Runner did not enroll” cascade | Inspect enrollment logs; retry once if evidence indicates the known restart flake. Repetition requires investigation. |
| Control-plane timeouts | Check host load and `QUALIFY_MAX_STACKS`; avoid concurrent suites. |
| Storage pressure admission denied | Check /tmp and workspace disk use; free only owned disposable data, never lower admission thresholds. |
| Firecracker websocket close 1006 | Compare shipped guest bundle provenance with guest commits; inspect the host firewall. |
| Wrong Go / mise version absent | Correct `RELEASE_GOROOT` in that runner's release.env. |
| Lint errors from another worktree | Use `GOLANGCI_LINT_CACHE=$PWD/.tmp/golangci-lint`. |
| npm 404 after publish | Poll with a five-minute bound, then inspect the matching workflow log. |
| Historical published body is an install one-liner | For an explicitly authorized repair, use `gh release edit vX.Y.Z --notes-file docs/releases/vX.Y.Z.md`; new publication preserves notes automatically. |
