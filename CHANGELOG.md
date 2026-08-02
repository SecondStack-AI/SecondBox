# Changelog

## Unreleased

- Replaced the editable 146-variable deployment environment with a strict versioned `secondbox.toml`, create-only initialization and Runner enrollment, redacted inspection, one-shot legacy migration, and atomically generated Compose and systemd environment artifacts.
- Split the deployment into explicit base, bundled-development, and privileged same-host Runner Compose overlays, with ambient configuration isolation and a one-command ready development control plane.
- Reduced the control-plane environment contract from 59 required variables to 38: 19 tuning settings are optional validated overrides with compiled reviewed values, and the Runner protocol window is now a verified compiled fact in both binaries.
- Added the standalone SecondBox control plane with PostgreSQL-backed projects, service accounts, API keys, immutable profiles, durable Sandboxes, quotas, lifecycle intent, bounded Assignment and stop reconciliation, runner scheduling, and end-to-end request correlation.
- Added home-Runner-pinned durable Workspaces with reflink-only local Snapshots, crash-safe in-place restore, durable generation receipts, exact-home scheduling, bounded drain cancellation, and Lease-aware activity reclamation.
- Added the outbound mTLS Runner protocol, Firecracker runner, independently versioned guest protocol, signed image pipeline, generation fencing, descriptor-pinned guest operations, fully correlated payload-free Runner operation evidence, and fenced natural-shutdown and cgroup OOM termination reporting.
- Added authenticated RunnerPool administration and credential-free Runner inventory through the API, generated clients, and CLI.
- Added durable buffered and backpressured WebSocket exec with binary stdin, explicit EOF, typed terminal outcomes, exact idempotency, reconnect delivery, cancellation proof, and assignment fencing, plus ordinary workspace filesystem APIs with binary transfer.
- Added durable PTY sessions with authenticated reconnect, exclusive bounded detach, ordered binary input and output, resize, Lease-aware cancellation proof, Go and TypeScript helpers, and the interactive `secondbox sandbox shell` command.
- Added authenticated, expiring Profile-approved port sessions with single-use control-plane WebSocket tunnels, bounded binary relay, generation and Lease fencing, and guest-loopback-only forwarding without Runner host-port publication.
- Added immutable Artifact upload, listing, metadata, verified download, retention deletion, quota enforcement, and S3-compatible garbage collection.
- Added fail-closed PostgreSQL and Artifact backup tooling with database-derived object manifests and verified checksums, plus an explicit operator recovery boundary for each Runner's stable identity and local workspace filesystem.
- Added generated Go, TypeScript, and Python clients, handwritten Go and TypeScript helpers, the `secondbox` CLI with bounded log streaming and support bundles, and a frozen Flue beta.9 compatibility adapter.
- Added clean-clone qualification, compatibility manifests, dependency-age enforcement, license evidence, checksums, signatures, provenance, and non-publishing release gates.
- Added fail-closed Runner storage-pressure admission for one dedicated reflink-capable workspace filesystem, with explicit hysteresis, bounded audit evidence, and generation-fenced cleanup.
