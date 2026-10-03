# SecondBox guest rootfs source

This directory defines the prepared filesystem source used to produce a
SecondBox Firecracker rootfs. The pipeline accepts one source of base filesystem
identity and never chooses a base implicitly:

- `SECONDBOX_RUNNER_MICROVM_OCI_BASE_REFERENCE` is a content-addressed OCI
  reference ending in `@sha256:<64 lowercase hex characters>`.
- `SECONDBOX_RUNNER_MICROVM_IMAGE_DEFINITION` is a path to a declarative
  `secondbox.microvm-rootfs` JSON definition. The committed Debian definition
  selects a dated snapshot and a guest-protocol compatibility range.

Setting neither input, setting both, using an OCI tag, or using an undated
Debian mirror fails before Docker or debootstrap runs. The output directory is
also explicit and must not already exist.

`SECONDBOX_RUNNER_MICROVM_ARCHITECTURE` is the required guest architecture,
`amd64` or `arm64`. An image definition must declare the same Debian
architecture. An OCI base is pulled for `linux/<architecture>`, and the build
fails when the resolved base or the built rootfs image has another platform.
The architecture is recorded in `rootfs-source-manifest.json`, and
`build.sh` refuses to sign a rootfs source of another architecture than its own
`SECONDBOX_RUNNER_MICROVM_ARCHITECTURE`.

## Inputs

| File | Purpose |
|------|---------|
| `secondbox-debian-image-definition.json` | amd64 guests: dated Debian snapshot, architecture, bootstrap set, and guest-protocol range |
| `secondbox-debian-image-definition-arm64.json` | arm64 guests: the same definition for the arm64 Debian userspace |
| `secondbox-apt-packages.txt` | packages resolved against the selected Debian package source |
| `secondbox-python-requirements.txt` | exact top-level Python requirements |
| `Dockerfile` | applies packages and configuration to the immutable base |
| `config/configure-imagemagick-policy.sh` | guest-local ImageMagick PDF/PS/EPS policy |
| `config/verify-debian-snapshot-sources.sh` | rejects OCI bases whose apt sources are not dated Debian snapshots |
| `verify-secondbox-rootfs.sh` | live, prepared-directory, and ext4 contract verification |
| `collect-secondbox-rootfs-provenance.py` | deterministic Debian and Python dependency/license evidence |
| `build-secondbox-rootfs-source.sh` | validates, builds, exports, and records provenance |

The Debian definition is an explicit build input even when using the committed
definition. Pair each definition with its guest architecture, and build the
microVM bundle from that rootfs source with the same
`SECONDBOX_RUNNER_MICROVM_ARCHITECTURE`:

```sh
# amd64 guests
SECONDBOX_RUNNER_MICROVM_ARCHITECTURE=amd64 \
SECONDBOX_RUNNER_MICROVM_IMAGE_DEFINITION="$PWD/runner/scripts/microvm-image/rootfs/secondbox-debian-image-definition.json" \
SECONDBOX_RUNNER_MICROVM_BROWSER_POLICY=forbid \
SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR="$PWD/tmp/secondbox-rootfs-source" \
runner/scripts/microvm-image/rootfs/build-secondbox-rootfs-source.sh

# arm64 guests
SECONDBOX_RUNNER_MICROVM_ARCHITECTURE=arm64 \
SECONDBOX_RUNNER_MICROVM_IMAGE_DEFINITION="$PWD/runner/scripts/microvm-image/rootfs/secondbox-debian-image-definition-arm64.json" \
SECONDBOX_RUNNER_MICROVM_BROWSER_POLICY=forbid \
SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR="$PWD/tmp/secondbox-rootfs-source" \
runner/scripts/microvm-image/rootfs/build-secondbox-rootfs-source.sh
```

`SECONDBOX_RUNNER_MICROVM_BROWSER_POLICY` is required: `forbid` rejects a
rootfs that ships a browser, `allow` records `browserPolicy: allow` in the
rootfs contract.
debootstrap runs natively, so build each architecture on a host of that
architecture.

An approved OCI base is supplied by immutable manifest digest:

```sh
SECONDBOX_RUNNER_MICROVM_ARCHITECTURE=amd64 \
SECONDBOX_RUNNER_MICROVM_OCI_BASE_REFERENCE="registry.example/secondbox/rootfs@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" \
SECONDBOX_RUNNER_MICROVM_BROWSER_POLICY=forbid \
SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR="$PWD/tmp/secondbox-rootfs-source" \
runner/scripts/microvm-image/rootfs/build-secondbox-rootfs-source.sh
```

`--validate-inputs-only` exercises source validation without Docker,
debootstrap, network access, or output creation.

## Released evidence

The exported rootfs contains `/usr/share/secondbox/image-provenance/` with:

- `rootfs-source-manifest.json`, including the immutable OCI digest or image
  definition hash plus hashes of every package/configuration input;
- `rootfs-debian-packages.lock`, containing exact resolved Debian versions;
- `rootfs-python.freeze`, containing the complete resolved Python environment;
- `rootfs-debian-license-inventory.json`, containing Debian source/version and
  copyright-file hashes;
- `rootfs-python-license-inventory.json`, containing Python version, license
  metadata, and metadata hashes.

The image release manifest must cover this provenance directory along with the
kernel, rootfs, and toolchain digests.
