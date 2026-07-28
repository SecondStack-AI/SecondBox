# Changelog

## Unreleased

- Added the standalone SecondBox control plane with PostgreSQL-backed projects, service accounts, API keys, immutable profiles, durable Sandboxes, quotas, lifecycle intent, bounded Assignment and stop reconciliation, runner scheduling, and end-to-end request correlation.
- Added portable immutable workspace checkpoints with verified S3-compatible publication, named stopped-state Snapshots, fenced cross-runner restore streaming, bounded drain cancellation, retention-aware garbage collection, and Lease-aware activity reclamation.
- Added the outbound mTLS Runner protocol, Firecracker runner, independently versioned guest protocol, signed image pipeline, generation fencing, descriptor-pinned guest operations, fully correlated payload-free Runner operation evidence, and fenced natural-shutdown and cgroup OOM termination reporting.
- Added authenticated RunnerPool administration and credential-free Runner inventory through the API, generated clients, and CLI.
- Added durable buffered and backpressured WebSocket exec with binary stdin, explicit EOF, typed terminal outcomes, exact idempotency, reconnect delivery, cancellation proof, and assignment fencing, plus ordinary workspace filesystem APIs with binary transfer.
- Added durable PTY sessions with authenticated reconnect, exclusive bounded detach, ordered binary input and output, resize, Lease-aware cancellation proof, Go and TypeScript helpers, and the interactive `secondbox sandbox shell` command.
- Added authenticated, expiring Profile-approved port sessions with single-use control-plane WebSocket tunnels, bounded binary relay, generation and Lease fencing, and guest-loopback-only forwarding without Runner host-port publication.
- Added immutable Artifact upload, listing, metadata, verified download, retention deletion, quota enforcement, and S3-compatible garbage collection.
- Added fail-closed backup and isolated restore-drill tooling with database-derived object manifests, portable checksums, restored-root verification, fresh-Runner database proof, and direct stale-generation rejection checks.
- Added generated Go, TypeScript, and Python clients, handwritten Go and TypeScript helpers, the `secondbox` CLI with bounded log streaming and support bundles, and a frozen Flue beta.9 compatibility adapter.
- Added clean-clone qualification, compatibility manifests, dependency-age enforcement, license evidence, checksums, signatures, provenance, and non-publishing release gates.
- Added fail-closed Runner storage-pressure admission for dedicated ext4, dm-thin, and restore-spool capacity, with explicit hysteresis, bounded audit evidence, and generation-fenced cleanup.
