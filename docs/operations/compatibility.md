# Compatibility status

`release/current-compatibility.json` is the only machine-readable authority for the current release candidate. It identifies the canonical contracts and records what has actually been qualified. The release package manifest hashes that file and every packaged contract and migration from the exact source commit; the compatibility file itself intentionally contains no transient hashes.

| Dimension | Current contract | Qualification |
| --- | --- | --- |
| Public API | OpenAPI v1 | v1 current-version behavior only; released-client skew is not qualified |
| Runner protocol | generation 1 | generation 1 only; adjacent-generation skew is not qualified |
| Guest protocol | generation 1 | generation 1 only; prior-generation skew is not qualified |
| Database | ordered PostgreSQL migrations | current schema is integration-tested; upgrade and rolling replacement are not qualified |
| Profile revisions | immutable persisted revisions | immutability is integration-tested; the schema is not versioned and reachable-revision upgrade is not qualified |
| Checkpoints | no released format version | current-version publication and restore are integration-tested; released-format and upgrade compatibility are not qualified |
| Artifacts | no released manifest version | current-version upload, download, retention, and authorization are integration-tested; released-manifest compatibility is not qualified |

These statuses are independent. Current-version API tests do not establish old-client compatibility. A descriptor fixture does not establish Runner or guest skew. Starting from the current database schema does not establish forward migration or rolling replacement. Current-version checkpoint and Artifact integration tests do not establish compatibility with a released storage format or manifest.

The repository also carries `tests/compatibility/initial-v1-release-candidate.json`, a hashed, explicitly unreleased baseline used by executable upgrade tests. Those tests exercise the frozen minimal v1 client against the current server, configurable Runner-version negotiation and fail-closed skew, guest-generation rejection before workspace mutation, exact migration adoption and drift rejection, immutable reachable Profile revisions, verified checkpoint streaming, and same-candidate control-plane replacement. They establish the initial release-candidate behavior without converting any absent adjacent released-version, prior-generation, or released-format scenario into qualification.

The release publication gate requires passing artifact evidence for every dimension. Adding a new descriptor, migration, Profile shape, checkpoint format, or Artifact manifest updates the canonical source contracts first, then requires package hashes and old/new scenario evidence for the supported window. An unqualified dimension remains explicit and blocks publication; it is never treated as implicitly compatible.

See the [compatibility policy](../design/compatibility-policy.md), [qualification gates](qualification.md), and [release evidence](release-evidence.md).
