---
name: worklog
description: Read and append SecondBox's shared local session ledger at session start, hand-off, release state changes, or discovery of a host or process gotcha.
---

# Worklog

Resolve the primary checkout from the first `worktree` record in
`git worktree list --porcelain`; `.worklog/` lives there, including when working
in another worktree. Create it if absent. Never commit it or use `git add -f`.

At session start read today's UTC file and the most recent earlier file, if
present. Read further back only when the task names an older effort. Treat
entries as historical evidence; verify live state before acting on them.

Append to `YYYY-MM-DD.md` at hand-off, at the end of any task longer than an
hour, after each release merge, launch, publish or retraction, and when a host
or process gotcha is found. Use UTC from `date -u`. Keep entries 5–15 lines:

```text
## 13:20Z · codex · main@1cda639 · release 0.14.0
Did: launched just release 0.14.0; unit secondbox-suite-release-0-14-0; run RUN
Result: test gate failed; scenario shards passed
Evidence: .tmp/release/RUN/timing.md; releases/0.14.0-build
Next: verify evidence and rerun gates before recovery
Gotcha: record a verified host or process constraint, or omit this line
```

Use `Did:`, `Result:`, `Evidence:`, `Next:` and optional `Gotcha:`. Record facts,
commit hashes, paths, run IDs, PRs, timing, and brief operator decisions. No
secrets, private key material, narration, or duplicated plan text. Reference
signing keys and password-manager items by name only.

Append each complete entry in one write, under `flock` on
`.worklog/.append.lock` when writers may overlap. Never rewrite earlier entries;
append a correction. Ignored ledger writes do not dirty a release checkout.
Codex implementors retain their `.codex-report.md` hand-off in their worktree;
the orchestrator distils it into this ledger. Durable conclusions also belong
in the completed plan's Outcome section and, for Claude only, Claude memory.
The ledger has no git backup; it is chronology, not the durable summary.
