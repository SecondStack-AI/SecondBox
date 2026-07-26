# MicroVM Image Pipeline

The Firecracker backend consumes three versioned artifacts:

- `kernel`: the guest kernel image.
- `rootfs.ext4`: the bootable agent root filesystem.
- `shared.img`: a read-only erofs/squashfs image for shared immutable content,
  or ext4 when local hosts do not have erofs/squashfs tooling.

Build locally with a supplied kernel:

```sh
AGENT_MANAGER_MICROVM_KERNEL_PATH=/path/to/vmlinux \
AGENT_MANAGER_MICROVM_KERNEL_CONFIG=/path/to/linux/.config \
just build-microvm-images
```

Build with the pinned kernel described by `scripts/microvm-image/kernel.lock`:

```sh
AGENT_MANAGER_MICROVM_BUILD_KERNEL=true \
just build-microvm-images
```

The pinned-kernel path downloads the exact kernel tarball from kernel.org,
verifies its SHA-256, builds `vmlinux` with reproducible Kbuild metadata, and
copies the kernel `.config` into the build output.

The build writes `kernel-provenance.json`, `rootfs-source-manifest.json`,
`manifest.json`, `SHA256SUMS`, `manifest.sig`, and `signing.pub` alongside the artifacts under
`releases/microvm/<version>/`.
`manifest.json` records `/init` as the guest entrypoint and
`/usr/local/bin/agent-manager-microvm-entrypoint` as the runtime bootstrap. The
manifest includes the kernel provenance and rootfs source-manifest hashes, so
the OpenSSL signature covers provenance as well as the artifact hashes.

Verify an artifact set with a trusted public key:

```sh
AGENT_MANAGER_MICROVM_OUT_DIR=releases/microvm/<version> \
AGENT_MANAGER_MICROVM_PUBLIC_KEY=releases/microvm/<version>/signing.pub \
just verify-microvm-images
```

For production, replace `AGENT_MANAGER_MICROVM_PUBLIC_KEY` with the pinned deployment
public key, not the artifact's bundled `signing.pub`. Omitting
`AGENT_MANAGER_MICROVM_PUBLIC_KEY` intentionally skips signature trust verification and
checks content integrity only.

The build pipeline:

- validates the supplied or built kernel config for virtio block/net/vsock, ext4, FUSE,
  user namespaces, and seccomp;
- copies the prepared guest rootfs from `AGENT_MANAGER_MICROVM_ROOTFS_SOURCE_DIR`;
- injects `agent-manager-microvm-agent`, `/init`, and the microVM entrypoint;
- creates an ext4 rootfs image and a shared image (`auto` prefers erofs, then
  squashfs, then ext4);
- scans the staged rootfs for forbidden runtime credential files and obvious
  baked secret material;
- signs the manifest with an OpenSSL key.

For generated images, set:

```sh
AGENT_MANAGER_MICROVM_KERNEL_PATH=/absolute/path/to/kernel
AGENT_MANAGER_MICROVM_ROOTFS_PATH=/absolute/path/to/rootfs.ext4
AGENT_MANAGER_MICROVM_SHARED_IMAGE_PATH=/absolute/path/to/shared.img
AGENT_MANAGER_MICROVM_KERNEL_ARGS="console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"
```

For local smoke on minimal hosts, force the portable shared-image fallback:

```sh
AGENT_MANAGER_MICROVM_SHARED_FORMAT=ext4
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

The old base-extension path remains available for emergency rollbacks:

```sh
AGENT_MANAGER_MICROVM_ROOTFS_SOURCE_MODE=extend just build-microvm-images-std
```

The package lists are the tracked source of truth; edit them and rerun to change
the set. Apt determinism comes from the locked Debian snapshot, and pip
determinism comes from pinned top-level requirements plus the generated freeze
file. See `scripts/microvm-image/rootfs/README.md`.
