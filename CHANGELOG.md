# Changelog

## Unreleased

- Replaced silent built-in Profile reconciliation with explicit, release-owned `agent-compartment` and `durable-coding` standard bundles, a shared idempotent resource apply engine, and strict deployment-owned RunnerPool inventory and gateway bindings.
- Added a coordinated `v0.1.0` release path for the existing Go and TypeScript SDKs, host binaries, digest-pinned OCI images, independently signed microVM artifacts, standard resources, SBOMs, provenance, qualification evidence, and an acyclic final release index.
- Expanded the Go and TypeScript SDKs with equivalent high-level lifecycle, execution, filesystem, Snapshot, Artifact, Lease, Port, and terminal operations; added Node transport helpers; and replaced the obsolete Flue beta.9 compatibility layer with the exact Flue 2 public contract.
- Added `secondbox-deploy` verification and release-aware production initialization, plus immutable candidate publication and no-checkout KVM qualification workflows that publish the final release index last.
- Replaced the editable 146-variable deployment environment with a strict versioned `secondbox.toml`, create-only initialization and Runner enrollment, redacted inspection, one-shot legacy migration, and atomically generated Compose and systemd environment artifacts.
- Split the deployment into explicit base, bundled-development, and privileged same-host Runner Compose overlays, with ambient configuration isolation and a one-command ready development control plane.
- Reduced the control-plane environment contract from 59 required variables to 38: 18 tuning settings are optional validated overrides with compiled reviewed values, and the Runner protocol window is now a verified compiled fact in both binaries.
- Added the standalone SecondBox control plane with PostgreSQL-backed projects, service accounts, API keys, immutable profiles, durable Sandboxes, quotas, lifecycle intent, bounded Assignment and stop reconciliation, runner scheduling, and end-to-end request correlation.
- Added home-Runner-pinned durable Workspaces with reflink-only local Snapshots, crash-safe in-place restore, durable generation receipts, exact-home scheduling, bounded drain cancellation, and Lease-aware activity reclamation.
- Added the outbound mTLS Runner protocol, Firecracker runner, independently versioned guest protocol, signed image pipeline, generation fencing, descriptor-pinned guest operations, fully correlated payload-free Runner operation evidence, and fenced natural-shutdown and cgroup OOM termination reporting.
- Added authenticated RunnerPool administration and credential-free Runner inventory through the API, generated SDK transports, and CLI.
- Added buffered and backpressured direct or proxied WebSocket exec with binary stdin, explicit EOF, typed terminal outcomes, exact idempotency, cancellation proof, and assignment fencing, plus ordinary workspace filesystem APIs with binary transfer.
- Added direct and proxied PTY sessions with authenticated reconnect, exclusive bounded detach, ordered binary input and output, resize, Lease-aware cancellation proof, Go and TypeScript helpers, and the interactive `secondbox sandbox shell` command.
- Added authenticated, expiring Profile-approved port sessions with single-use proxied WebSocket or scoped direct tunnels, generation and Lease fencing, and guest-loopback-only forwarding without Runner host-port publication.
- Removed the unused PostgreSQL data-plane frame transport, per-frame wakeups and retention, relay-only configuration and harnesses, and the frame/payload schema; every Port, Exec, File, and PTY payload now uses `SBXDP1` directly or through the in-memory proxy.
- Added immutable Artifact upload, listing, metadata, verified download, retention deletion, quota enforcement, and S3-compatible garbage collection.
- Added fail-closed PostgreSQL and Artifact backup tooling with database-derived object manifests and verified checksums, plus an explicit operator recovery boundary for each Runner's stable identity and local workspace filesystem.
- Added generated Go and TypeScript transports and wire types, handwritten Go and TypeScript helpers, the `secondbox` CLI with bounded log streaming and support bundles, and a frozen Flue beta.9 compatibility adapter.
- Added clean-clone qualification, compatibility manifests, dependency-age enforcement, license evidence, checksums, signatures, provenance, and non-publishing release gates.
- Added fail-closed Runner storage-pressure admission for one dedicated reflink-capable workspace filesystem, with explicit hysteresis, bounded audit evidence, and generation-fenced cleanup.
