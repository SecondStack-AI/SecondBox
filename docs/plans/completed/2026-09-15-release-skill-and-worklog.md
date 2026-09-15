---
title: Release skill, worklogs, and shared skill layout
date: 2026-09-15
status: implemented locally (including `release.sh --resume`); live release validation pending
owner: SecondStack
provenance: SecondBox releases 2026-08-20 to 2026-09-15, assessed from git/gh, Claude session logs, and Codex session logs
---

# Plan: `secondbox-release` skill, `.worklog/` ledger, shared skill layout

## Outcome

Two project-level skills that both Claude Code and Codex load from one canonical
directory, a git-ignored daily worklog that either harness reads at start and
appends at hand-off, and the documentation corrections the skills depend on.
No release mechanics move: `just release` stays the only way to cut a release,
and the skills encode the judgement and checklists around it.

Evidence labels in Part 1: **V** verified in git, gh, or files; **L** from session
logs (session ids given); **I** inferred.

# Part 1: What the last four weeks of releasing looked like

## 1.1 Throughput

| | |
|---|---|
| Published GitHub releases 2026-08-20 to 2026-09-15 | 16 (v0.5.2 to v0.14.0) (V) |
| Tags created | 22; 7 never published: v0.8.0, v0.8.1, v0.8.1-fix, v0.8.1-fix.2, v0.8.2, v0.8.2-fix.1, v0.9.0 (V) |
| Go module retractions in `go.mod` | v0.7.0, [v0.8.0, v0.8.2], v0.9.0 (V) |
| Published tags not on `main` | v0.13.0, tagged on PR #141's unmerged branch; tree identical to the re-landed commit (V, L 01a0a0c9) |
| Releases with `docs/releases/vX.md` | 10 of 14 since v0.5.2; missing v0.10.0, v0.10.1, v0.11.0, v0.13.0 (V) |
| `CHANGELOG.md` last dated section | `0.4.4 - 2026-08-08`; everything since sits under `## Unreleased` (V) |
| Merge-to-publish before `just release` | v0.6.0 about 9 h, v0.7.0 overnight then retracted, v0.8.x about 4 h for four tags, v0.10.0 about 3 h (L) |
| With `just release` (since 2026-09-13) | v0.11.0 31m32s staged; v0.12.0 11m13s lean; v0.13.0 about 2.5 h tag-to-publish after three failed runs; v0.14.0 15 min merge-to-publish after one failed run (V run dirs, L) |

## 1.2 What went wrong, ranked by cost

1. **Tag first, qualify later, then move or retract the tag.** v0.7.0 was re-pointed
   five times; proxy.golang.org cached the first commit, so v0.7.1 exists only to carry
   `retract v0.7.0`. v0.8.0 to v0.8.2 and v0.9.0 repeated the pattern (L 01a03fce,
   01a05af3, 01a05b21, 01a05b96; V go.mod). `just release` fixed this structurally: it
   creates a local tag and prints the push command only after staging succeeds.
2. **Low-level scripts bypass the "from clean main" rule.** v0.13.0 was cut with
   `git tag v0.13.0 1645f10` on the unmerged #141 branch plus `just release-upload`,
   caught only after publication ("HOW you published 13 if you didn't merge 141?").
   `release.sh` enforces `HEAD == main`; `git tag` and `release-upload` do not.
3. **PRs merge green in CI and fail on real backends.** #128 and #134 both broke
   Firecracker behaviour while green; each was found by release qualification, not by
   the PR (L 69e5634f). CI runs no real backend; self-hosted runners remain deferred.
4. **Release-host state drift**: deleted trust-anchor directory (v0.10.0 needed a hunt
   through old transcripts for a fingerprint), deleted private signing key (v0.12.0
   rotation, every deployment must reinstall), guest bundle five weeks behind the guest
   agent, disk at 91%, `release.env` missing a new key, arm64 binfmt lost on reboot, UFW
   and libvirt ACLs. Seven incidents (L). `deploy/release.env.example` still shows the
   pre-rotation anchor `40a61101…` and bundle path while v0.12.0+ uses `59c127f4…` (V).
5. **Commit-exact evidence on a serial manual chain**: five one-file fixes on 2026-09-12
   each invalidated about 50 minutes of evidence ("Today it took six hours"). Solved by
   `just release`; a post-merge fix now costs one lean run of about 10 minutes.
6. **Host contention and orphaned state**: another session's scenario suite mistaken
   for a control-plane crash (five wasted reproducer rounds, L 297af6f0), orphaned
   `sbq-*` guests, `secondbox-suite-*` stacks, leftover `VERSION-build` directories, a
   checkout lock held by a dead run (v0.13.0 attempt 2). Nine or more incidents.
7. **Harness killing background work** (Claude memory heuristic, Codex sandbox, session
   boundaries): five incidents, answered first by `setsid nohup`, then by
   `systemd-run --user`. The incantation was rebuilt by hand each time.
8. **Salvage by hand after a partial `just release` failure.** v0.14.0's run failed on
   the `test` gate alone (six unit tests failing at 0.00s, a 26-second flake) while all
   eight scenario shards passed and both evidence files were already commit-exact. The
   session re-ran the gates with `just qualify --tier release --only gates`, then stepped
   through candidate, installer, and final stage by hand from the retained build (V run
   dirs and evidence timestamps). Correct, but unscripted and unrepeatable.
9. **Publisher discards release notes.** `scripts/release-publish.sh:35` flips the draft
   with `gh release edit --notes "Guided Linux amd64 install: …"`, which replaces whatever
   body the draft had. Present since PR #25; first visible on v0.14.0, the first release
   whose draft carried real notes before the workflow ran (V; L 01a0a4dc).
10. **Agent judgement errors caught only by the operator**: the source-only
    "qualification waiver" publish of v0.6.0, and the v0.13.0 tag-before-merge.
11. **Documentation skipped under time pressure**: no changelog cut since 0.4.4; four
    releases without notes; `release-distribution.md` still documents the manual chain;
    `downstream-release-integration.md` is hardcoded to v0.10.0 (V).

## 1.3 What worked

- `just release` (PRs #131, #135, #140) removed the manual chain, the early tag push,
  env recovery from old release directories, and serial ordering.
- `retract` directives and `docs/releases/v0.12.0.md` handled bad tags and the key
  rotation honestly.
- Claude memory carried release knowledge between Claude sessions.
- Codex implementor briefs converged on a stable shape: read AGENTS.md, name the plan,
  commit without pushing, write `.codex-report.md`, launch long commands under
  `systemd-run`.

## 1.4 Continuity today, and the gap

- Claude: `~/.claude/projects/…/memory/*.md`. Rich, invisible to Codex, written only when
  a session remembers to.
- Codex: nothing persistent. Continuity came from pasted hand-off prompts, off-repo
  `RESUME.md` and `SECONDSTACK-RELEASE-HANDOFF.md` files, `.codex-report.md` from
  implementors, and once from grepping old transcripts for a fingerprint.
- Repo: `docs/plans/*.md` Outcome and proof sections, and `.tmp/release/RUN/timing.md`,
  the only machine-written release record; the v0.11.0 run directory is already gone.
- SecondStack has no worklog convention (V). Its nearest analogues are the git-ignored
  `.plans/` directory of local plan and hand-off notes and the tracked `docs/plans/`.
  This design adopts that shape and names it.

# Part 2: Design

## 2.1 Skill layout

```
.agents/skills/
  secondbox-release/
    SKILL.md
    agents/openai.yaml          # Codex display metadata, as in SecondStack's skills
  worklog/
    SKILL.md
    agents/openai.yaml
.claude/skills/
  secondbox-release -> ../../.agents/skills/secondbox-release   # relative symlink, committed
  worklog           -> ../../.agents/skills/worklog
CLAUDE.md                        # one line: @AGENTS.md   (SecondStack's pattern; SecondBox has none)
AGENTS.md                        # plus the six lines in 2.4
.gitignore                       # plus /.worklog/
```

- Codex discovers `<repo>/.agents/skills` natively; Claude discovers `<repo>/.claude/skills`.
  SecondStack commits the symlinks (mode 120000), so a clone reproduces the layout.
- `.agents` is the only canonical direction here. SecondStack is mixed (two skills
  canonical in `.agents`, four in `.claude`); flipping its four is a separate chore.
- No `.codex/` directory. No skill-local scripts: every command the skills need exists
  in `Justfile` and `scripts/`, and a skill carrying its own shell would drift from them.
- `CLAUDE.md` is needed because Claude Code loads `CLAUDE.md`; today SecondBox's rules
  reach Claude only through global memory.

## 2.2 `secondbox-release` skill

Trigger description: release, cut, tag, publish, retract, "issue a new release", release
notes, bundle rotation, or anything that pushes a `v*` tag in this repository.

Division of labour: `docs/operations/release-operator-setup.md` stays the mechanics
reference (what `just release` does, one-time host setup, manual appendix). The skill
owns what the docs do not: invariants learned from Part 1, pre-flight and close-out
checklists, failure triage, verification, and the worklog hand-off. The skill links to
the doc rather than repeating it.

### Invariants

1. A release is cut from `origin/main` at a merged commit, by `just release VERSION`, on
   the release host. Never `git tag` plus `just release-upload` by hand, never a PR
   branch. (v0.13.0)
2. The tag is pushed only with the command `just release` prints after staging. A pushed
   tag is immutable; a bad release is retracted in `go.mod`, noted in the changelog and
   release notes, and followed by the next patch. (v0.7.0, v0.8.x, v0.9.0)
3. One release on the host at a time. Before launching, check `secondbox-suite-*` user
   units, `sbq-*` libvirt domains, `secondbox-suite-*` containers,
   `.tmp/release/checkout.lock`, and leftover `RELEASE_OUTPUT_ROOT/VERSION{,-build,-candidate}`.
   Never stop another operator's work; report and wait. (2026-08-26, v0.13.0)
4. The release checkout is frozen from launch to `stage`: no branch switch, no edits, no
   untracked files. `release.sh` fails the run otherwise. Notes and worklog entries are
   written elsewhere until it finishes, or the dedicated release worktree is used (D3).
5. Long commands run under `systemd-run --user --unit=secondbox-suite-release-VERSION …`
   exactly as the operator doc shows; poll `systemctl --user is-active` at most every five
   minutes, or use a Monitor. A plain background process may be killed by the harness.
6. Never publish partial artifacts, waivers, or source-only releases. If qualification
   cannot pass, the answer is "not released", with the failing stage named. (v0.6.0)
7. Release notes and the changelog cut land on `main` before the tag in a
   `docs(release): prepare vX.Y.Z` PR, even pre-1.0. Skipping is an explicit operator
   decision, recorded in the worklog. (v0.11.0, v0.13.0)
8. When `just release` fails after the scenario stages passed, the salvage path is the
   documented resume sequence in the skill (gates-only requalification, then candidate,
   installer, stage from the retained build), never an ad hoc reconstruction. Until
   `release.sh --resume` exists (2.5), the skill spells the sequence out. (v0.14.0)
9. Secrets never enter the repo, notes, or worklog: the private signing key and its
   1Password item are referenced by name only.

### Procedure

0. **Orient**: read the last two worklog days, the operator doc,
   `gh release list --limit 5` (not `git tag`, which lists dead tags), and
   `git log --oneline vLAST..origin/main`.
1. **Version**: pre-1.0 policy: minor when a protocol generation, migration checksum,
   bundle or trust anchor, or clean-install boundary changes; patch otherwise. Confirm
   with the operator only when the evidence is ambiguous.
2. **Pre-flight** (read-only): invariant 3; `release.env` keys diffed against
   `deploy/release.env.example`; trust-anchor fingerprint equals what the notes claim;
   `df` headroom (200 GiB rule) and `MemAvailable`; `npm ci --ignore-scripts` freshness;
   for `--full` only: VM idle and arm64 binfmt registered.
3. **Notes PR** on branch `release/vX.Y.Z`: turn `## Unreleased` into `## X.Y.Z - DATE`
   and leave a fresh empty `## Unreleased`; write `docs/releases/vX.Y.Z.md` from the
   v0.14.0 file as template (Deployment boundary, bundle identity, sections per area,
   Distribution); update any boundary or upgrade doc; bump the version references in
   `downstream-release-integration.md`. Open the PR to `main`, wait for CI, merge (D6).
4. **Launch**: `git switch main && git pull --ff-only`, confirm HEAD is the merge commit,
   then the `systemd-run … /usr/bin/just release X.Y.Z` line from the doc. Append a
   worklog entry with run id, unit name, HEAD, and start time. Poll.
5. **Triage on failure** with the table below. Clean up only this run's leftovers. A code
   fix keeps the notes commit valid, but the local tag is deleted (`git tag -d vX.Y.Z`),
   the fix lands on `main` first, and the release is relaunched. A gate-only failure with
   passed scenario stages takes the resume sequence (invariant 8).
6. **Publish** with the two printed commands in order: `git push origin refs/tags/vX.Y.Z`,
   then `just release-upload X.Y.Z OUTPUT`. Release notes are supplied to the upload
   (2.5, notes fix); the skill never edits the GitHub release body afterwards. Then
   `gh run list --workflow release.yml --limit 1` and `gh run watch --exit-status`.
7. **Verify**: `gh release view vX.Y.Z --json isDraft,isPrerelease,body,assets` (not
   draft, 29 assets lean or 30 full as of v0.12.0 to v0.14.0, body is the release notes
   and not the placeholder); `npm view @secondstack-ai/secondbox@X.Y.Z version`;
   `GOFLAGS=-mod=mod GOPROXY=https://proxy.golang.org go list -m github.com/SecondStack-AI/SecondBox@vX.Y.Z`
   from an empty module cache; `docker buildx imagetools inspect` on the six GHCR tags;
   `releases/latest/download/install.sh` embeds the new version.
8. **Close out**: worklog entry with the `timing.md` table, published coordinates, and
   lessons; Claude also updates memory; the downstream hand-off block that
   `downstream-release-integration.md` asks for (manifest URL and digest, `SHA256SUMS`
   digest, npm integrity, OCI digests, Profile revision digests, protocol window) plus a
   statement of whether existing deployments can update or must reinstall. Remove
   `VERSION-build`, `VERSION-candidate`, and any worktrees the release created.

### Failure triage table (seed)

| Symptom | Cause | Action |
|---|---|---|
| preflight `output already exists` | previous failed run | remove only this version's `-build`, `-candidate`, final directories |
| preflight `another release owns this checkout` | dead run left `checkout.lock`, or a live run | `systemctl --user list-units 'secondbox-suite-*'`; remove the lock only if none is active |
| preflight `libvirt has sbq- domains` | orphaned installer guest | `virsh -c qemu:///system destroy` and `undefine --nvram` only if the owner unit is gone |
| preflight missing `QUALIFY_GVISOR_HOST_BUILD_ROOT` or key mismatch | `release.env` predates a change | diff against the example; fix the env, never the script |
| `test` gate: several tests fail at 0.00s within seconds | disposable Postgres not ready or environment leak (v0.14.0, v0.13.0) | `just qualify --tier release --only gates`; if green, resume per invariant 8; if it repeats, it is real |
| `test` gate `…UnsafeConfiguration/group_or_other_readable` | file modes in the checkout or env directory | fix modes; do not relax the test |
| Firecracker shard `Runner did not enroll` cascade | runner-restart flake (about 1 in 4 before #140) | rerun once; a repeat on the same commit is real |
| shard control-plane HTTP timeouts | stack starvation | check `QUALIFY_MAX_STACKS`; run nothing else concurrently |
| `storage pressure admission denied` | `/tmp` or workspace root full (bundle build, disk at 91%) | free space; never lower the threshold |
| direct exec `websocket: close 1006` on Firecracker only | guest change not in the shipped bundle, or host firewall | compare bundle vintage with merged guest commits; check the firewall scripts |
| arm64 build stage fails (`--full`) | binfmt lost on reboot | re-register per memory; verify `docker buildx inspect` |
| `mise` "No version is set" or wrong Go | shim | `release.sh` pins `RELEASE_GOROOT`; export the pinned version for manual steps |
| golangci reports errors from another worktree | shared cache | `GOLANGCI_LINT_CACHE=$PWD/.tmp/golangci-lint` |
| npm 404 after publish | registry propagation | bounded polling, five minutes, then read the workflow log |
| published release body is the install one-liner | publisher overwrote the notes (until the fix in 2.5 lands) | `gh release edit vX.Y.Z --notes-file docs/releases/vX.Y.Z.md` once |

### `agents/openai.yaml`

```yaml
interface:
  display_name: "SecondBox Release"
  short_description: "Cut, publish, verify, or retract a SecondBox release with just release"
  default_prompt: "Use $secondbox-release to cut and publish the next SecondBox release from origin/main."
```

## 2.3 `worklog` skill and convention

Purpose: one chronological ledger both harnesses read at start and append to at hand-off,
so the next session, Claude or Codex, starts from what happened on this host rather than
from a pasted brief or a transcript grep.

```
.worklog/                     # git-ignored; lives in the primary checkout only
  2026-09-15.md               # one file per UTC day, append-only
```

Entry shape (5 to 15 lines; the skill enforces the headings, not the prose):

```
## 13:20Z · claude · main@1cda639 · release 0.14.0
Did: merged #144 (notes); launched `just release 0.14.0` as unit secondbox-suite-release-0-14-0, run 20260915T132015-2629912
Result: test gate failed (6 unit tests at 0.00s); all shards passed; evidence commit-exact
Evidence: .tmp/release/20260915T132015-2629912/, releases/0.14.0-build
Next: gates-only requalification, then candidate → installer → stage from the build
Gotcha: release-publish.sh overwrites draft notes with the install one-liner
```

Rules:

- Resolve the directory from the primary worktree (`git worktree list --porcelain | head -1`),
  so worktrees and Codex implementors append to the same ledger.
- Read at session start: today and the previous file. Read further back only when the
  task names an older effort.
- Write at hand-off, at the end of any task longer than an hour, after every release step
  that changes shared state (merge, launch, publish, retract), and whenever a gotcha is
  found. Codex implementors keep writing `.codex-report.md` in their worktree; the
  orchestrator distils it into the worklog.
- Content: facts, run ids, paths, commit hashes, PR numbers, timings, and operator
  decisions quoted briefly. No secrets, no narration, no restating plan docs.
- Durable facts also go to Claude memory (Claude only) and to the relevant
  `docs/plans/<plan>.md` Outcome section when an effort completes. The worklog is the
  chronology, not the summary.
- Never commit it, never `git add -f` it (SecondStack's `.plans/` rule).

Why untracked rather than `docs/worklog/`: the repository is public and the ledger
carries host paths, run ids, and operational detail; tracked entries would need a PR
each and would mix into feature PRs; SecondStack's precedent is untracked. Cost: it lives
on one host without git backup. Mitigation: it sits on the same filesystem as memory and
release outputs, and durable facts are copied out.

`agents/openai.yaml`: display name "Worklog", short description "Read and append the
shared session ledger in .worklog/", default prompt "Use $worklog to record what this
session did and what the next session must know."

## 2.4 AGENTS.md additions

```
- Project skills live in `.agents/skills/` (Codex) with symlinks in `.claude/skills/`
  (Claude). `secondbox-release` owns cutting, publishing, verifying, and retracting
  releases; `worklog` owns the shared session ledger.
- Read `.worklog/` (today and the previous day) at session start and append an entry at
  hand-off, after any release step, and whenever you discover a host or process gotcha.
  The directory is git-ignored; never commit it.
- Releases are cut only by `just release VERSION` from a clean `origin/main` checkout;
  never push a `v*` tag or run `release-upload` outside that flow.
```

## 2.5 Repository changes the skills depend on

Documentation, in the same PR as the skills:

- `deploy/release.env.example`: update the anchor to `59c127f4…` and the bundle source
  to `bundles/secondbox-0.12.0`, or state that the example is intentionally historical.
- `docs/operations/release-distribution.md` Publishing: replace the manual chain with
  `just release`; point to the operator doc appendix for hosts without automation.
- `docs/operations/downstream-release-integration.md`: make version-neutral, or make the
  bump a step in the notes PR (the skill assumes the latter).
- `CHANGELOG.md`: cut `## Unreleased` per D4.

Code, in a separate small PR before the skill is first used:

- **Release notes survive publication.** `scripts/release-upload.sh` takes an optional
  `NOTES_FILE` argument, defaulting to `docs/releases/vVERSION.md` when present at the
  tag and otherwise to the placeholder; it creates the draft with that body and appends
  the install and SDK one-liner. `scripts/release-publish.sh` drops `--notes` so the
  draft body is what gets published. `release.sh` prints the notes path in its publish
  commands. Gate: `just test-release-workflow` plus a dry run against a draft.

Code follow-ups, not blocking:

- `release.sh --resume` (done 2026-09-15): reuse a retained `VERSION-build` and
  commit-exact evidence, requalify only the gates, then continue. Encodes the v0.14.0 salvage.
- Ancestry guard in `release-upload.sh`: refuse a tag that is not on `origin/main`.
- `just release-status`: print units, locks, and leftover output directories.
- Self-hosted runner so `just qualify` runs per PR (already deferred in the
  fast-qualification plan).

## 2.6 Decisions

Resolved:

- **D1 Skill name**: `secondbox-release`. Avoids the user-level `/release` command and
  SecondStack's `releasing`, which means changelog grooming there.
- **D2 Worklog placement**: untracked `.worklog/` in the primary checkout.
- **D5 v0.13.0 off-main tag**: leave it; the tree is identical to the re-landed commit
  and downstream already pins it. Record in the worklog; no retraction.

Open, with recommendation:

- **D3 Dedicated release worktree**: a permanent worktree kept on `main` so a running
  release never freezes the checkout you develop in. Recommended; today's run froze the
  primary checkout for 15 minutes. If adopted, `release.env` names it in a comment and
  the skill checks it.
- **D4 Changelog backfill** (resolved 2026-09-15): full backfill. Every release from
  0.4.5 to 0.14.0 has its own dated section, the four missing notes files were written
  after the fact, and the GitHub release bodies the publisher had overwritten were
  regenerated from the sections with a fenced install footer.
- **D6 Notes-only PRs**: merge on green CI without waiting for Dark Review
  (recommended; SecondStack's rule) vs always wait.
- **D7 SecondStack link direction**: flip its four `.claude`-canonical skills to
  `.agents`-canonical now, or leave it.

## Implementation order

1. Code PR: release notes fix (2.5). Small, testable, unblocks step 6 of the skill.
2. Skills PR: layout (2.1), both `SKILL.md` files, `openai.yaml`, `CLAUDE.md`, AGENTS.md
   lines, `.gitignore`, and the three doc corrections. First worklog entry written by
   hand as the seed.
3. Exercise both skills on the next release from a fresh Claude session and a fresh Codex
   session; adjust wording from what each harness actually did.
4. Follow-ups from 2.5 as separate PRs, `--resume` first.

## Implementation outcome — 2026-09-15

Implemented locally:

- Upload accepts an optional notes file, otherwise reads the release notes from
  the immutable local tag. It appends the install/SDK footer and refreshes draft
  notes on retries. Publication preserves the draft body; the release driver's
  printed instructions identify the tagged notes path.
- Added canonical `secondbox-release` and `worklog` skills with Codex metadata,
  relative Claude symlinks, `CLAUDE.md`, repository guidance and an ignored ledger.
- Updated bundle, independent anchor and both component pins together from the
  documented v0.12.0 identities. Example bundle/trust paths are explicitly
  illustrative and require operator provisioning.
- Replaced the distribution manual chain with `just release`, clarified the
  operator appendix and service launch, and made downstream coordinates
  version-neutral (including optional lean-release pod evidence).
- Adopted D4's consolidated 0.15.0 changelog section, explicitly marked as not
  evidence of publication. The skill handles that existing section on release
  preparation without creating a duplicate version heading.

Decision dispositions: D3 remains an operator host-setup choice; this change does
not create or reassign worktrees. D6 follows existing review/merge authorization
without adding a notes-only review exemption. D7 is outside this repository's
scope. D5 remains unchanged and is recorded in the local ledger.

Inspection corrected two details in the proposed skill: `checkout.lock` is a
kernel-held `flock`, so a leftover file must not be treated as an active or stale
lock; gates-only recovery must explicitly load the original release environment,
toolchain, tier/platform selection and evidence paths. Recovery holds the same
checkout lock and stops on any failed stage.

Publication/PR sequencing remains a hand-off concern: these local changes have
not opened or merged PRs, pushed tags, created a GitHub draft, or run a release.
The notes code and skills can be split for the two proposed PRs. A live GitHub
draft test and fresh Claude/Codex skill exercises remain for the next authorized
release. `--resume`, upload ancestry enforcement, `release-status`, and hosted
backend CI remain separate follow-ups as planned.

### Verification

Passed: `just verify-generated`, `just test` (with a dedicated disposable
PostgreSQL container, subsequently removed), `just test-contract`, `just
test-compose` (live Go/TypeScript SDK smoke), `just test-install-docs`, and
`just test-release-workflow`. Both skills passed `quick_validate.py`; relative
links, Claude symlinks, recovery shell syntax and `git diff --check` passed.
The workflow regression runs the actual upload/publish scripts with a local
fake GitHub transport; it covers tagged versus dirty-checkout notes, explicit
notes, draft retries without footer duplication, absent explicit files,
placeholder fallback, public-release refusal and body preservation.

Additional `just test-release-stage` is **not green**. A frozen-checkout trace
reached and passed candidate/build equivalence and manifest verification, then
failed the existing `agent-compartment` revision assertion: the test expects
`[1,2,3,4]`, while the unchanged standard-resource generator emits `[1,2,3,4,5]`.
Revision 5 adds the unlimited lifecycle ceiling and maximum duration. The
stager, standard-resource generator and this assertion are unchanged by this
patch. Diagnostic trace: `/tmp/secondbox-release-skill-stage-frozen-trace.log`;
retained JSON: `/tmp/secondbox-release-skill-stage-diagnostics/`. An earlier run
was also invalidated by edits during execution; the frozen rerun isolated this
separate stale expectation. No assertions were weakened or skipped.

No runner protocol, reconciliation or workspace-durability code changed, so
qualified backend scenario runs were not performed. Live GitHub draft behavior
and fresh-harness skill discovery/execution remain unverified as noted above.
