# Sandbox Host MicroVM Image Pipeline

The Firecracker backend consumes three versioned artifacts:

- `kernel`: the guest kernel image.
- `rootfs.ext4`: the bootable agent root filesystem.
- `shared.img`: a read-only erofs/squashfs image for shared immutable content,
  or ext4 when local hosts do not have erofs/squashfs tooling.

Build locally with a supplied kernel:

```sh
SANDBOX_HOST_MICROVM_KERNEL_PATH=/path/to/vmlinux \
SANDBOX_HOST_MICROVM_KERNEL_CONFIG=/path/to/linux/.config \
just build-microvm-images
```

Build with the pinned kernel described by `scripts/microvm-image/kernel.lock`:

```sh
SANDBOX_HOST_MICROVM_BUILD_KERNEL=true \
just build-microvm-images
```

The pinned-kernel path downloads the exact kernel tarball from kernel.org,
verifies its SHA-256, builds `vmlinux` with reproducible Kbuild metadata, and
copies the kernel `.config` into the build output.

The build writes `kernel-provenance.json`, `rootfs-source-manifest.json`,
`manifest.json`, `SHA256SUMS`, `manifest.sig`, and `signing.pub` alongside the artifacts under
`releases/microvm/<version>/`.
`manifest.json` records `/init` as the guest entrypoint and
`/usr/local/bin/sandbox-tool-entrypoint` as the runtime bootstrap. The
manifest includes the kernel provenance and rootfs source-manifest hashes, so
the OpenSSL signature covers provenance as well as the artifact hashes.

Verify an artifact set with a trusted public key:

```sh
SANDBOX_HOST_MICROVM_OUT_DIR=releases/microvm/<version> \
SANDBOX_HOST_MICROVM_PUBLIC_KEY=releases/microvm/<version>/signing.pub \
just verify-microvm-images
```

For production, replace `SANDBOX_HOST_MICROVM_PUBLIC_KEY` with the pinned deployment
public key, not the artifact's bundled `signing.pub`. Omitting
`SANDBOX_HOST_MICROVM_PUBLIC_KEY` intentionally skips signature trust verification and
checks content integrity only.

## Release distribution and host materialization

The signed bundle is approximately 11 GB and is not embedded in the source-less GitHub release zip. A tagged release reads an independently signed bundle from the absolute runner path configured by `SANDBOX_HOST_MICROVM_RELEASE_SOURCE_DIR`, verifies it against the repository-variable fingerprint `SANDBOX_HOST_MICROVM_RELEASE_PUBLIC_KEY_SHA256`, and publishes the exact ten-file allowlist as the dedicated `sandbox-host-microvm-artifacts:<release-tag>` scratch image. The image labels bind the verified public-key fingerprint and `manifest.json` digest. The release workflow copies both identities into the staged `.env.template`.

Initialization accepts exactly one explicit source mode:

- `directory` reads `SANDBOX_HOST_MICROVM_ARTIFACT_SOURCE_DIR`. Source checkouts use this mode and resolve a relative directory against the repository root.
- `ghcr-image` pulls `SANDBOX_HOST_MICROVM_ARTIFACT_IMAGE`, validates its identity labels, and extracts `/sandbox-host-microvm` through a temporary container.

There is no automatic selection or fallback between modes. Both paths verify the exact file allowlist, the independently pinned signing-key fingerprint, payload checksums, manifest signature and hash bindings, and verified standard-toolset state before atomically replacing an empty target at `SANDBOX_HOST_MICROVM_ARTIFACTS_DIR`. A non-empty target is accepted only when it is the same verified bundle; a different target fails init. Matching GHCR labels allow repeat init to verify the installed target without extracting the 11 GB image again.

Docker access belongs to the host init process. The Agent Manager container remains an unprivileged control plane and never receives the Docker socket or artifact-distribution credentials.

The build pipeline:

- validates the supplied or built kernel config for virtio block/net/vsock, ext4, FUSE,
  user namespaces, and seccomp;
- copies the prepared guest rootfs from `SANDBOX_HOST_MICROVM_ROOTFS_SOURCE_DIR`;
- injects `sandbox-guest-agent`, `/init`, and the microVM entrypoint;
- creates an ext4 rootfs image and a shared image (`auto` prefers erofs, then
  squashfs, then ext4);
- scans the staged rootfs for forbidden runtime credential files and obvious
  baked secret material;
- signs the manifest with an OpenSSL key.

For generated images, set:

```sh
SANDBOX_HOST_MICROVM_KERNEL_PATH=/absolute/path/to/kernel
SANDBOX_HOST_MICROVM_ROOTFS_PATH=/absolute/path/to/rootfs.ext4
SANDBOX_HOST_MICROVM_SHARED_IMAGE_PATH=/absolute/path/to/shared.img
SANDBOX_HOST_MICROVM_KERNEL_ARGS="console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"
```

For local smoke on minimal hosts, force the portable shared-image fallback:

```sh
SANDBOX_HOST_MICROVM_SHARED_FORMAT=ext4
```

## Standard package set (reproducible rootfs source)

`scripts/microvm-image/rootfs/` bakes a standard package set for coding and
productivity workloads into the tool-VM (matplotlib/pandas/scipy, office + PDF
libraries, OCR, LibreOffice headless, fonts, …) so agents do not pay a runtime
install delay. By default it creates a fresh Debian bookworm rootfs using
`debootstrap` and the pinned snapshot in
`scripts/microvm-image/rootfs/debian-rootfs.lock`, then applies `apt-std.txt`,
the pinned `requirements-std.txt`, and `config/`.

```sh
just build-microvm-images-std
```

This prepares `tmp/microvm-rootfs-src` with debootstrap and Docker, then writes
`rootfs-source-manifest.json`, `rootfs-packages.dpkg.lock`, and
`rootfs-python.freeze` into that source directory. The signed-image builder
copies the source manifest into the artifact directory and covers it from
`manifest.json`.

The base-extension source mode builds a signed image by extending the configured base artifact:

```sh
SANDBOX_HOST_MICROVM_ROOTFS_SOURCE_MODE=extend just build-microvm-images-std
```

The package lists are the tracked source of truth; edit them and rerun to change
the set. Apt determinism comes from the locked Debian snapshot, and pip
determinism comes from pinned top-level requirements plus the generated freeze
file. See `scripts/microvm-image/rootfs/README.md`.

## CI artifact evidence

CI publishes artifact provenance through the Host-owned evidence command:

```sh
go run ./cmd/sandbox-host-artifact-evidence \
  --artifacts "$SANDBOX_HOST_MICROVM_OUT_DIR" \
  --out /path/to/new/firecracker-artifacts.json
```

The output contains only the fixed artifact allowlist, byte sizes, and SHA-256
digests. It rejects symlink inputs and refuses to overwrite existing evidence.
