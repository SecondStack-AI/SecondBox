# Client-selected execution images

## Purpose

A SecondBox application selects an execution image for each Sandbox create or start operation.
The image contains one signed Firecracker bundle in the `/secondbox-runner-microvm` directory.
The Runner downloads the image, verifies the bundle, and starts the new Instance with that bundle.

The Sandbox Profile continues to control resources, network policy, placement, and guest protocol compatibility.
The image request cannot change those controls.
The Sandbox workspace remains on its home Runner and survives an image change.

## Public contract

The create and start request bodies contain a required `image` object.
The object contains a fully qualified OCI reference and optional pull credentials.
The credentials are separate from the reference.

A tag resolves once for each new lifecycle Operation.
The Runner records the resolved digest before it downloads the bundle.
Retries of the same Operation use that digest.
A new Operation resolves the tag again.

The public Sandbox and Instance records contain the requested reference and resolved digest.
The public Operation never contains pull credentials.

## Credential custody

The control plane keeps pull credentials in a bounded memory broker.
It binds one credential to one lifecycle Operation ID.
The durable assignment record contains only the non-secret image reference.
The control plane adds the credential when it sends the assignment on the authenticated Runner stream.

A control-plane restart can remove a credential before the Runner receives it.
The Operation then fails with a credential-required error.
The client must submit a new lifecycle request with current credentials.

The Runner writes credentials to a temporary Skopeo auth file with mode `0600`.
The Runner removes the file after the registry operation.
The guest never receives the credential.
The Runner does not use a global Docker login.

## Retrieval and trust

Each Runner has an explicit registry-host allowlist.
The allowlist prevents a client from using the Runner as a credentialed network proxy.
The Runner uses normal TLS certificate validation.
An operator can install a private CA at `registry-certificates/<registry-host>/ca.crt` in the Runner state directory.
SecondBox has no insecure-TLS option.

Client-selected images have an explicit signing public key and fingerprint in Runner configuration.
This trust root is separate from the fixed release bundle trust configuration, although a release installer can deliberately set both to the release key.

The Runner resolves the reference through the registry before each new Operation.
This authorization check also occurs when the digest is already in the local cache.
One tenant cannot use another tenant's cached private bytes without valid registry access.

The Runner downloads an exact digest with Skopeo.
It extracts only the fixed bundle directory from the OCI layers.
It verifies the existing checksum, signature, trust-anchor, and rootfs contracts.
The Runner publishes the cache directory only after all checks pass.
The Runner records the verified kernel, rootfs, and shared-image file identities and passes those exact identities to Firecracker staging.
Any replacement between verification and staging fails the assignment.

The cache key is the resolved digest.
A per-digest file lock serializes publication on one Runner.
Incomplete staging directories are not valid cache entries.
Explicit operator limits bound the downloaded archive, expanded bundle, and retained cache.
The Runner evicts the least recently used complete images before a cold pull and refuses preparation when the configured free-space reserve is unavailable.

## Backend scope

The first implementation supports cold Firecracker starts on Linux amd64.
The Runner advertises `client-selected-image` only when it supports this path.
The scheduler requires that feature for an image-selected assignment.

Snapshot-resume Profiles reject client-selected images.
Snapshot templates contain one exact rootfs and cannot safely accept another bundle.
The gVisor and Microsandbox backends also reject this contract until they implement and qualify their own materialization paths.

## Workspace and upgrade behavior

An image change creates a new Instance against the same workspace disk.
SecondBox does not migrate files, repair dependencies, run upgrade hooks, or restore the previous image automatically.
The caller must stop a running Sandbox before it selects another image.

The caller can select an older digest later.
This changes the execution tools only.
It does not revert workspace changes.

## Progress and failure

The Runner reports image resolution, download, extraction, verification, and VM start stages through assignment progress.
The lifecycle Operation remains asynchronous during this work.
Its assignment deadline covers registry resolution, cache locking, download, extraction, verification, workspace attachment, and VM launch.

Registry authentication, registry availability, invalid references, failed signatures, corrupt bundles, and unsupported backends fail explicitly.
SecondBox does not select a different image and does not use an unverified directory.
