# Compatibility policy

SecondBox versions its contracts and durable formats independently. Compatibility in one dimension does not imply compatibility in another.

## Public API

The public HTTP contract is OpenAPI 3.1 under `/v1`. Additive optional response fields and new endpoints are compatible within v1. Removing or renaming fields, changing meanings, making optional request fields required, broadening mutation defaults, or changing stable error codes requires a new API major.

Generated clients are tested against every supported control-plane minor release. A server ignores no unknown request property because request schemas are closed; a newer client must feature-detect server support before sending a newer operation.

## Runner protocol

The protobuf runner protocol negotiates its own version at connection start. The release policy targets the current and immediately preceding released runner protocol generation during rolling upgrades. A newer Runner uses only the negotiated schema and features. Unsupported skew fails before registration or assignment.

Committed descriptors and binary fixtures are checked for protobuf wire compatibility. Application API version changes do not force runner-protocol changes.

The current baseline has only generation 1 descriptor and fixtures, so it qualifies generation 1 only. The prior-generation window cannot be claimed until a second released generation and cross-version connection fixture exist.

## Guest-agent protocol

The release policy targets the current and two immediately preceding released guest protocol generations. Negotiation freezes one generation and feature set for the connection. A signed image declares its generation and required features. Unsupported guests fail before Instance readiness or workspace mutation.

The broader guest window accounts for stopped durable checkpoints that refer to older released images. Dropping the oldest generation requires release qualification of rejection-before-mutation or an explicit offline checkpoint/image migration.

The current baseline has only generation 1 descriptor and fixtures, so no guest skew is qualified yet.

## Database

Database migrations are ordered, immutable after release, and forward-only for production upgrade. Rolling control-plane replacement supports the documented adjacent-version window. A migration does not rely on a new binary until every old replica is unable to write an incompatible shape.

Database compatibility is internal. Clients and Runners never depend on table or row layouts.

## Profiles

A ProfileRevision is immutable and pinned to each Sandbox. New Profile revisions do not migrate existing Sandboxes. A binary release remains able to interpret every reachable revision or rejects startup before scheduling. Removing support requires an explicit operator migration and reachability proof.

## Checkpoints and snapshots

Checkpoint manifests name format version, architecture, content hashes, ProfileRevision, immutable execution assets, Firecracker compatibility, guest protocol generation, and required features. Restore validates all dimensions before creating mutable local state. Format conversion, when supported, writes a new immutable checkpoint; it never rewrites the source.

Snapshots share checkpoint disk-state compatibility and preserve no RAM, process, Lease, PTY, or network compatibility.

## Artifacts

Artifact bytes are opaque to SecondBox. Manifest compatibility covers checksum algorithm, size, media type, retention, and authorization metadata. An Artifact remains downloadable while retained even if its source Sandbox or Profile is disabled.

## Release evidence

Every release freezes:

- the OpenAPI document and generated-client hashes;
- runner and guest descriptors plus golden fixtures;
- database migration set;
- supported ProfileRevision schema versions;
- checkpoint and Artifact manifest schemas;
- image, binary, SDK, and guest-asset digests.

Upgrade tests cover old client to new server, new client feature detection, adjacent Runner skew, every supported guest generation, rolling control-plane replacement, reachable Profile revisions, and restore of every supported checkpoint format.

Before the first public release, `tests/compatibility/initial-v1-release-candidate.json` freezes an explicitly unreleased executable baseline. It lets the migration and replacement mechanisms be tested without pretending that an adjacent released binary, prior protocol generation, or older durable format already exists. Release qualification remains bounded by the machine-readable statuses below.

`release/current-compatibility.json` is the only machine-readable compatibility authority. It records qualified generations and the exact qualification state of every independent dimension without embedding mutable source-tree hashes. Release packaging hashes the current OpenAPI document, descriptors, fixtures, migrations, and compatibility manifest together in the package manifest. The compatibility authority records checkpoint and Artifact paths as integrated only for the current implementation while released-format compatibility remains unqualified; rolling control-plane replacement and database upgrade are `not-qualified`, Profile schemas are `not-versioned`, and only protocol generation 1 is qualified. File hashes detect changes; they do not substitute for old/new binary, migration, Runner, guest, checkpoint, or rolling-replica scenarios.

See [API conventions](api-conventions.md), [Runner protocol](runner-protocol.md), [Guest-agent protocol](guest-agent-protocol.md), and [Workspace durability](workspace-durability.md).
