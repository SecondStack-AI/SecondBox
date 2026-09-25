# Client-selected execution images

## Ownership and public contract

The application owns its userspace image and builds it with a pinned SecondBox builder.
The signed bundle includes the existing guest agent, not the public `secondbox` CLI.
SecondBox uses the existing signed manifest and guest handshake; it does not require matching builder and host release numbers.

Create accepts an optional `image.reference`, a fully qualified OCI tag or digest.
Omitting the image selects the Profile's fixed assets; an explicitly malformed image is rejected.
Start also accepts an optional image.
Omitting the image on start reuses the Sandbox's pinned digest, resolved after idempotency replay inside the admission transaction.
Before the first preparation, it uses the explicit image recorded at creation.
The first verified preparation establishes the initial digest pin, even if guest startup later fails.
Persisted Sandboxes without a selected-image identity retain their original immutable Profile assets.

A newly accepted tag selection resolves once per Operation.
Replays and retries retain that digest; a distinct explicit tag selection can resolve newer content.
For a later explicit replacement, the successful Instance updates the Sandbox pin.
An unsuccessful replacement does not replace the last successful pin.
The Profile still controls resources, placement, networking, and lifecycle policy.
Image selection does not rewrite historical Profile revisions.

## Retrieval boundary and credential custody

Each Runner host runs an unprivileged image fetcher in a separate container.
The fetcher executable shares the Runner release image, but receives no Workspace mounts, host devices, Docker socket, or privileged capabilities.
It receives only the image cache, a host-private Unix socket, registry configuration, and publisher public key.
The privileged Runner handles shared storage reservations and verifies local files before launch; it does not retrieve registry content or extract archives.

The operator configures one registry authentication record per Tenant on each eligible host.
Lifecycle and preparation requests contain neither credentials nor credential selectors.
The record grants exact repositories and selects anonymous access, username/token access, or an exported Docker login file.
Docker login authentication supports encoded username/password credentials, including JSON-key passwords, and identity tokens.
External credential helpers are rejected because their executables and desktop state do not belong in the fetcher.

The fetcher reads Tenant configuration for each preparation, including cache hits.
It also checks the host-wide registry allowlist and performs registry authorization before reusing cached bytes.
An explicit empty auth document prevents anonymous access from inheriting a host login.
Temporary auth files are private and removed after use.
Credentials never enter guest files, Operations, or durable Runner commands.
TLS verification is mandatory; private CAs reside under the configured `certificates/<registry-host>/ca.crt` directory.

## Signed metadata and launch authority

Preparation uses existing lifecycle effects and durable Runner command delivery.
The fetcher streams progress and returns bounded signed metadata through its local socket.
It has no independent job database or retry scheduler.
The control plane verifies the existing bundle signature with its configured publisher key before constructing an assignment from the signed component identities.
The assignment contains a digest reference and authorized identities, never a mutable tag or pull credentials.

The Runner independently verifies local bundle bytes and checks the assigned component identities before guest negotiation.
Full verification hashes gigabytes, so the Runner verifies each bundle once and then admits later starts of the same bundle on the recorded filesystem identity of every verified file.
Changed metadata forces one re-verification, and a Runner restart re-verifies each bundle it starts, the fixed Profile bundle included.
A materialization report cannot overwrite assignment authority.
Captured-file identity checks reject replacement between verification and backend staging.
Failed verification does not select another image.

## Preparation without compute

`POST /v1/images:prepare` accepts an image and optional Profile and returns the existing asynchronous Operation.
It requires lifecycle authority and uses the intersection of Tenant and application Profile grants.
Admission captures a finite set of ready eligible Runners; a Profile narrows that set.
The service resolves the reference once, verifies signed metadata, and sends that digest to compatible members of the captured set.
Resolution runs on the captured Runner with the widest architecture coverage, and that fetch counts as its own preparation, so no Runner retrieves the same digest twice.
Fleet members added later are not silently added to the Operation.

The initial bounds are 16 target Runners and a 30-minute deadline.
An eligible Runner set above the target bound is rejected as an invalid request that must select a narrower Profile, because waiting cannot reduce that set.
Preparation consumes the existing Tenant and Subject concurrent-operation quota but allocates no Sandbox, Workspace, or VM.
Operation inspection reports the image digest and target/completion counts, including the captured target count before the reference resolves.
Every target reports before the Operation decides.
It succeeds only when all targets prepared the digest, and otherwise fails naming the Runners that did not, while the counts still report which Runners hold the image.
An expired deadline fails the Operation rather than claiming complete coverage.

## Cache and storage

The shared cache is keyed by digest, but access remains Tenant-authorized.
A Firecracker cache must share a filesystem with the microVM run directory, which the Runner checks at startup, because a start stages the selected rootfs by reflink and image bytes are never copied.
A gVisor Runner proves at startup that its cache filesystem can reflink into an unnamed file, for the same reason.
Cross-process digest locks serialize publication and prevent eviction during local verification and launch.
Only complete, verified directories enter the cache.
Prepared entries retain a bounded expiry marker until the preparation deadline to close the preparation-to-launch eviction window.
Preparation is not a permanent cache-residency promise.
Eviction removes a digest lock with its bytes, and a lock acquired on an unlinked file is taken again, so cache metadata does not outlive the cache.
A recorded tag resolution expires one day after its Operation, which cannot be replayed beyond its deadline.

Operator limits bound compressed download size, expanded bundle size, and retained cache size.
Cold retrieval is serialized and can evict unpinned least-recently-used entries.
The fetcher alone admits staging capacity, because only it knows whether a tag resolves to cached bytes, and it measures the cache filesystem that receives them.
A cold retrieval reserves the worst-case staging size and evicts unpinned entries other than its own digest until the cache filesystem admits it.
A warm preparation reserves nothing and cannot be refused for capacity.
The Runner therefore charges Workspace and Instance storage pressure for Sandbox disks only.

## Backend and workspace behavior

The qualified selected-image backends are cold Firecracker and gVisor on Linux amd64.
Only a supporting Runner advertises `client-selected-image`.
Snapshot-resume Profiles and Microsandbox reject selected-image assignments until their materialization paths are implemented and qualified.

## gVisor materialization

gVisor consumes the same signed OCI artifact as Firecracker; applications publish one image for both backends.
Retrieval, registry allowlist, Tenant authorization, signature and fingerprint checks, the cache, and its limits are the shared implementation above.
The runner protocol does not change.
Generation 5 already carries `RUNNER_FEATURE_CLIENT_SELECTED_IMAGE`, and a gVisor Runner reports `client-selected-image` readiness like Firecracker, which admits it to `images:prepare` targets and selected-image placement.
Every gVisor Runner requires the image fetcher, cache root, and publisher key; there is no fixed-assets-only gVisor mode.

The image supplies userspace only.
The Runner uses the signed `rootfs.ext4` as the sandbox root and ignores the signed kernel, `shared.img`, `/init`, and the guest agent embedded in the image.
The Runner's pinned materialization still supplies `runsc` and the guest agent, which runs as PID 1 exactly as with fixed assets.
An assignment's runtime and toolchain assets must equal the image's signed components, the materialization agent must speak the assigned guest protocol generation, and it must implement every mandatory guest feature.
The guest handshake reports the image's signed component digests and the materialization's build identity.

A start verifies local bytes under the digest lock through the shared memoized verifier.
The Runner then opens `rootfs.ext4`, requires the open file to match the verified identity, reflinks it into an unnamed inode on the cache filesystem, and releases the digest lock.
Cache eviction after that point cannot affect the Instance.
The mount supervisor attaches the clone to an auto-clearing loop device in its private mount namespace and mounts it `nosuid,nodev`.
It creates only absent gVisor mount targets under the flat-root contract, then remounts the root read-only.
`runsc` serves it read-only behind the same in-memory overlay as the fixed flat root.
A root that violates the flat-root contract, such as a symlinked `/etc/resolv.conf`, fails the start; the Runner never repairs it.
Teardown unmounts the root after the Workspace detaches.
The unnamed clone is freed with its last descriptor, so a crashed Runner leaves no named staging file to sweep.

A warm start costs one metadata identity check, one reflink, and one loop mount; a cold start first runs the existing preparation effect.
The clone shares extents with the cache entry, and only mount metadata and created targets diverge.
It is not charged against Workspace capacity.
Guest writes to the root go to the sentry's memory overlay, which the Instance memory limit bounds.

The trust boundary equals Firecracker's: the same publisher key, allowlist, and Tenant authorization admit the same bytes.
One difference remains: the host kernel parses the signed ext4 root, where Firecracker leaves that to the guest kernel.
The gVisor backend already mounts each guest-writable Workspace ext4 image in the host kernel, so a signed, verified, private, read-only root adds no new parser class.
The guest reaches the root only through the gofer.

An image change boots a new Instance against the same Workspace disk.
The caller stops active compute before requesting another image.
SecondBox does not migrate files, repair dependencies, run upgrade hooks, or automatically restore an older image.
Selecting an older digest later does not revert Workspace changes.
