# SecondBox Runner MicroVM Image Pipeline

The Firecracker backend consumes three versioned artifacts:

- `kernel`: the guest kernel image.
- `rootfs.ext4`: the bootable guest root filesystem.
- `shared.img`: a read-only erofs/squashfs image for shared immutable content,
  or ext4 when local hosts do not have erofs/squashfs tooling.

`runner/scripts/microvm-image/build.sh --help` lists every required build input. The builder has no environment defaults. For a supplied kernel, set both the kernel path and its config path explicitly and set `SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL=false`. For the pinned kernel, set `SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL=true`, explicitly set the kernel path and config variables to empty strings, and provide the four kernel-builder inputs shown by `build-kernel.sh --help`.

The pinned-kernel path downloads the exact kernel tarball from the locked URL, verifies its SHA-256, builds `vmlinux` with reproducible Kbuild metadata, and copies the kernel `.config` into the build output.

The build writes `kernel-provenance.json`, `rootfs-source-manifest.json`, `secondbox-rootfs-contract.json`, the package and license inventories, `manifest.json`, `SHA256SUMS`, `manifest.sig`, and `signing.pub` alongside the artifacts in the explicitly configured output directory.
`manifest.json` records `/init` as the guest entrypoint and
`/usr/local/bin/secondbox-runner-guest-entrypoint` as the runtime bootstrap. The
manifest includes the kernel provenance and rootfs source-manifest hashes, so
the OpenSSL signature covers provenance as well as the artifact hashes.

The same signed rootfs boots two ways, selected by kernel argument. A tenant
Instance boots with its full `secondbox.*` assignment identity. A
snapshot-resume template boots with `secondbox.template_mode=1` and no identity
at all: the guest serves only the host-only vsock control endpoint, leaves
`/dev/vdb` unmounted, and refuses every guest-protocol connection. Such a guest
receives its Sandbox identity and mounts its Workspace through one
`POST /assignment/bind` control request, which it refuses before
`POST /restore/harden` succeeds and refuses for every request after the first.
Template capture is performed only inside the privileged runner qualification
boundary; the runner never boots a tenant Instance in template mode.

Verify an artifact set with an independently trusted public key and its canonical DER SHA-256 fingerprint:

```sh
just -f runner/Justfile verify-microvm-images \
  releases/microvm/<version> \
  /etc/secondbox/trust/artifact-signing.pem \
  <64-lowercase-hex-fingerprint>
```

The verifier never trusts the artifact's bundled `signing.pub`. The trusted key and fingerprint are mandatory; an unsigned bundle or a missing, malformed, or mismatched trust anchor fails verification.

## Release distribution and host materialization

The signed bundle is approximately 11 GB and is not embedded in the source-less GitHub release zip. Local release preparation reads an independently signed bundle from the absolute path configured by `SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR`, verifies it against `SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256`, and builds the exact allowlist as the dedicated `microvm-artifacts` OCI archive. The hosted publisher pushes that supplied archive without rebuilding it. Image labels and the artifact manifest bind the verified public-key fingerprint and `manifest.json` digest.

For a manual deployment, materialize the release's digest-pinned
`microvm-artifacts` image into the operator-selected artifact directory and
verify it with the independent public key before enrolling the Runner. Set
`artifact_host_directory` and the matching trust and asset pins in the Runner
declaration; [deployment operations](deployment.md) documents that contract.
`secondbox-deploy runner-init` issues Runner identity and configuration, not
execution assets.

The [guided installer](guided-single-host-install.md) materializes the image
from the verified release manifest. It extracts into a create-only temporary
directory, verifies the fixed allowlist and signed component identities against
the release-bound fingerprint, and publishes the directory atomically. Resume
rechecks and reuses a recorded verified directory. A different bundle needs a
separate target and a coordinated deployment transition.

Docker access belongs to host preparation. The Runner consumes already verified
assets and receives no artifact-distribution credentials.

The build pipeline:

- validates the supplied or built kernel config for virtio block/net/vsock, ext4, FUSE,
  user namespaces, and seccomp;
- copies the prepared guest rootfs from `SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR`;
- injects `secondbox-guest-agent`, `/init`, and the microVM entrypoint;
- creates an ext4 rootfs image and a shared image (`auto` prefers erofs, then
  squashfs, then ext4);
- scans the staged rootfs for forbidden runtime credential files and obvious
  baked secret material;
- signs the manifest with an OpenSSL key.

Configure the runner to consume the verified materialized bundle with:

```sh
SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH=/absolute/path/to/kernel
SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH=/absolute/path/to/rootfs.ext4
SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH=/absolute/path/to/shared.img
SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS="console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init"
SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT=1024
SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT=1025
SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL=5s
SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY=/etc/secondbox/trust/artifact-signing.pem
SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256=<64-lowercase-hex-fingerprint>
```

For local smoke on minimal hosts, force the portable shared-image fallback:

```sh
SECONDBOX_RUNNER_MICROVM_SHARED_FORMAT=ext4
```

## Standard package set (reproducible rootfs source)

The [rootfs source directory](../../runner/scripts/microvm-image/rootfs/README.md)
defines the coding and document-processing toolset. Select exactly one explicit
immutable source: a content-addressed OCI reference or the committed declarative
Debian image definition. There is no default source selection.

Set the required inputs described by that directory and
`runner/scripts/microvm-image/build.sh --help`, then run:

```sh
just -f runner/Justfile build-microvm-images-std
```

`secondbox-apt-packages.txt`, `secondbox-python-requirements.txt`, and `config/`
are the package/configuration inputs. The builder writes the source manifest,
Debian package lock, Python freeze, and license inventories. The signed image
manifest binds those digests. Edit the tracked inputs and rebuild to change
the toolset; updating the Runner binary alone does not change a signed guest.

## CI artifact evidence

From the `runner/` module, record artifact evidence with:

```sh
go run ./cmd/secondbox-artifact-evidence \
  --artifacts "$SECONDBOX_RUNNER_MICROVM_OUT_DIR" \
  --out /path/to/new/firecracker-artifacts.json
```

The output contains only the fixed artifact allowlist, byte sizes, and SHA-256
digests. It rejects symlink inputs and refuses to overwrite existing evidence.
