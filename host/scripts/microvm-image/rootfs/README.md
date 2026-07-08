# Standard tool-VM rootfs

These files define the **standard package set** baked into the agent tool-VM
rootfs so that common coding and productivity/cowork workloads run without paying
a runtime install delay (e.g. an agent asked to "generate an image" no longer has
to `pip install matplotlib` mid-turn and blow the per-turn timeout).

## What this is

The default path builds a fresh Debian bookworm rootfs source from
`debootstrap`, using the pinned Debian snapshot in `debian-rootfs.lock`. It then
imports that base into Docker, applies `apt-std.txt`, pinned
`requirements-std.txt`, and `config/`, and exports a prepared source directory
for `scripts/microvm-image/build.sh`.

The legacy "extend the current signed rootfs" path is still available with
`AG_MICROVM_ROOTFS_SOURCE_MODE=extend` for emergency rollbacks, but it is no
longer the default because it depends on an opaque, gitignored
`releases/microvm/latest/rootfs.ext4`.

## Files

| File | Purpose |
|------|---------|
| `debian-rootfs.lock` | pinned Debian suite, architecture, snapshot mirror, and debootstrap include set |
| `apt-std.txt` | apt packages to add (Debian bookworm): native tools + `python3-*` libs |
| `requirements-std.txt` | pip-only Python libs not packaged in Debian |
| `Dockerfile` | applies the two lists + config bakes on top of `${BASE_IMAGE}` |
| `config/imagemagick-relax-policy.sh` | re-enable ImageMagick PDF/PS/EPS coders |
| `config/profile.d-agentcy-std.sh` | headless matplotlib env (`MPLBACKEND=Agg`) |
| `build-rootfs-source.sh` | orchestrates import → docker build → export to a dir |

## Delivery model

Importable Python libraries go into the **system interpreter** (`python3`): apt
`python3-*` for everything Debian packages, plus a thin `pip --break-system-packages`
layer (build-time only) for the handful of pip-only packages. This extends the
image's existing apt-`python3-*` convention rather than introducing a shadowing
venv, so `python3 script.py` imports the standard stack with no PATH games.
Runtime installs of anything *not* pre-baked still prefer `uv` (`uv run`, `uv pip`).

Determinism is split by package source:

- apt packages resolve through the snapshot mirror in `debian-rootfs.lock`;
- pip top-level packages are pinned in `requirements-std.txt`;
- each build writes exact resolved outputs as `rootfs-packages.dpkg.lock` and
  `rootfs-python.freeze`;
- `rootfs-source-manifest.json` records the mode, lock-file hashes, snapshot
  mirror, and generated lockfile names. The signed artifact manifest covers this
  source manifest.

## Building

Two steps — prepare the rootfs source dir, then run the existing signed-image
builder against it:

```sh
# 1. Prepare the rootfs source directory (needs debootstrap, docker, sudo/root,
#    and network egress).
scripts/microvm-image/rootfs/build-rootfs-source.sh   # -> tmp/microvm-rootfs-src

# 2. Build signed kernel/rootfs/shared artifacts from it (reuses the current kernel).
AG_MICROVM_ROOTFS_SOURCE_DIR=tmp/microvm-rootfs-src \
AG_MICROVM_KERNEL_PATH=releases/microvm/latest/kernel \
AG_MICROVM_ROOTFS_SIZE_MIB=10240 \
scripts/microvm-image/build.sh
```

Or via `just build-microvm-images-std`, which chains both.

To use the legacy extension path:

```sh
AG_MICROVM_ROOTFS_SOURCE_MODE=extend \
AG_MICROVM_BASE_ROOTFS=releases/microvm/latest/rootfs.ext4 \
scripts/microvm-image/rootfs/build-rootfs-source.sh
```

Adjust the package set by editing `apt-std.txt` / `requirements-std.txt` and
rebuilding. A bad package name fails the docker build loudly (no silent drops).
