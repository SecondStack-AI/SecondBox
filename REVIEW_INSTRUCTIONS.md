# Dark Review Instructions

`AGENTS.md` is the primary engineering and review authority. `docs/design/` and `contracts/openapi/` hold the contract authority, `docs/operations/` the operator contracts.

- Report concrete correctness, security, data-integrity, durability, and availability risks introduced by this pull request.
- Per `AGENTS.md`, remove replaced code rather than retain a parallel execution path, placeholder backend, or fallback execution. Versioned decoding of durable published artifacts is not such a path: recorded install plans and receipts, deployment and release manifests, and qualification evidence must stay decodable for every release still inside the supported update window.
- `docs/releases/` names the oldest release a guided update accepts; deployments on earlier releases are recreated, not upgraded. Report any change that makes an artifact written by a still-accepted release undecodable, or that alters a recorded digest, and name the release it breaks.
- Migrations are forward-only, and a released migration's contents are its identity: `migrations/postgres/migrations.go` validates recorded checksums positionally. A release that requires a fresh database says so in its `docs/releases/` note. Report a rewritten released migration, a migration that duplicates or contradicts the baseline, a fresh-initialization failure, and code that disagrees with the schema it reads.
- `docs/operations/` runbooks are operator contracts: report an instruction that is wrong, unrunnable as written, or contradicted by the code. Do not ask `docs/operations/microsandbox-macos.md` for a complete supported enrollment, deployment, or service-management procedure; that backend is experimental and has no supported deployment path.
- On a later revision, review the changes made since the last reviewed revision and their consequences.
