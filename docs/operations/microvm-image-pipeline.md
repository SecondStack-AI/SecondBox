# SecondBox Runner MicroVM Image Pipeline

The Firecracker backend consumes three versioned artifacts:

- `kernel`: the guest kernel image.
- `rootfs.ext4`: the bootable guest root filesystem.
- `shared.img`: a read-only erofs/squashfs image for shared immutable content,
  or ext4 when local hosts do not have erofs/squashfs tooling.

`runner/scripts/microvm-image/build.sh --help` lists every required build input. The builder has no environment defaults. For a supplied kernel, set both the kernel path and its config path explicitly and set `SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL=false`. For the pinned kernel, set `SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL=true`, explicitly set the kernel path and config variables to empty strings, and provide the five kernel-builder inputs shown by `build-kernel.sh --help`.

`SECONDBOX_RUNNER_MICROVM_ARCHITECTURE` selects the guest architecture, `amd64` or `arm64`; it sets the guest agent build, the manifest architecture, and the kernel requirements.
The pinned-kernel path downloads the exact kernel tarball from the locked URL, verifies its SHA-256, builds the Firecracker boot image with reproducible Kbuild metadata, and copies the kernel `.config` into the build output.
The boot image is `vmlinux` on amd64 and the PE `Image` on arm64; `kernel-required-<architecture>.config` adds the Firecracker platform devices of that architecture.
`SECONDBOX_RUNNER_MICROVM_KERNEL_CROSS_COMPILE` is the kernel `CROSS_COMPILE` toolchain prefix. It may be empty to use the host compiler, which then must run on the guest architecture; the build refuses an empty prefix on a host of the other architecture.
The prefix and the compiler identity are recorded in `kernel-provenance.json`.

### Guest kernel Docker build

`runner/scripts/microvm-image/kernel.Dockerfile` builds the same pinned kernel with only Docker on the host.
Its build context is `runner/scripts/microvm-image`, and the target platform selects the guest architecture:

```sh
docker buildx build --platform linux/arm64 \
  -f runner/scripts/microvm-image/kernel.Dockerfile \
  --output type=local,dest=/absolute/kernel-arm64 \
  runner/scripts/microvm-image
```

The build stage runs on the build platform and cross-compiles with the Debian toolchain for the target, so an arm64 host builds the amd64 kernel and an amd64 host builds the arm64 kernel without emulation.
Both native and cross builds use the triplet-prefixed compiler, so the kernel records one compiler identity per target.
Repeated builds of the same inputs produce identical kernel and config bytes on any build host; only `createdAt` in `kernel-provenance.json` differs.
Only `kernel.lock`, `build-kernel.sh`, `check-kernel-config.sh`, and the `kernel-required*.config` files enter the build, so other source changes keep the Docker layer cache; the source download depends only on `kernel.lock`.
The final `scratch` stage holds exactly the `build-kernel.sh` output under `/kernel`: the boot image (`/kernel/vmlinux` on amd64, `/kernel/Image` on arm64), `/kernel/config`, `/kernel/System.map`, and `/kernel/kernel-provenance.json`.
There is no architecture-neutral alias; consumers select the boot image name from the architecture they already pass to the bundle builder.
The provenance `builder.gitCommit` is empty because the build context has no Git metadata.

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

## Client execution image builder

`scripts/build-client-execution-image.sh` converts one digest-pinned OCI userspace into a signed execution image.
The script uses the same rootfs, guest-agent, kernel, manifest, and signature pipeline as release artifacts.
It then packages the exact artifact allowlist under `/secondbox-runner-microvm` in an OCI image.

Use `deploy/client-execution-image-builder.Dockerfile` when the build host does not have the required build tools.
The builder runs on a Linux amd64 or arm64 Docker daemon. It runs the rootfs build steps of the target architecture, so build each architecture natively on a host of that architecture. It needs privileged loop-device and mount access, the Docker socket, and enough free space for the source layers, rootfs image, signed bundle, and final OCI build context.
Run that container with the Docker socket, a writable output directory, the signing key and public key, and the source kernel directory mounted.
The guest kernel Docker build output is a suitable kernel directory: set `SECONDBOX_CLIENT_IMAGE_KERNEL_PATH` to its boot image and `SECONDBOX_CLIENT_IMAGE_KERNEL_CONFIG` to its `config`.
Set every `SECONDBOX_CLIENT_IMAGE_*` variable explicitly.
The source reference must contain a digest.
The source commit must be the exact SecondBox revision used by the builder image.
The prepared source image must contain Python 3 and pip because the rootfs inventory records the installed Python environment.
The builder always uses prepared OCI mode.
`SECONDBOX_CLIENT_IMAGE_BROWSER_POLICY` is `forbid` or `allow`. `forbid` fails the build when the rootfs contains a browser package, launcher, or runtime; `allow` builds a userspace that ships a browser on purpose and records `browserPolicy: allow` in the signed rootfs contract.
Both output directories must be absent before the build starts.
`SECONDBOX_CLIENT_IMAGE_ARCHITECTURE` is `amd64` or `arm64` and must match the builder's Docker platform and the supplied kernel. The builder pulls and builds the source image for `linux/<architecture>`, refuses a source or rootfs image of another platform before signing, and packages the execution image for that platform.

This example builds an amd64 execution image on an amd64 Docker host. On an arm64 host, use `linux/arm64`, `SECONDBOX_CLIENT_IMAGE_ARCHITECTURE=arm64`, and the arm64 `Image`:

```sh
docker buildx build --platform linux/amd64 --load \
  -f deploy/client-execution-image-builder.Dockerfile \
  -t secondbox-client-image-builder:local .

mkdir -p /absolute/output-parent
docker run --rm --privileged --platform linux/amd64 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /absolute/output-parent:/output \
  -v /absolute/kernel-parent:/kernel:ro \
  -v /absolute/signing-parent:/signing:ro \
  -e SECONDBOX_CLIENT_IMAGE_ARCHITECTURE=amd64 \
  -e SECONDBOX_CLIENT_IMAGE_ARTIFACT_VERSION=local \
  -e SECONDBOX_CLIENT_IMAGE_BROWSER_POLICY=forbid \
  -e SECONDBOX_CLIENT_IMAGE_BUNDLE_DIR=/output/bundle \
  -e SECONDBOX_CLIENT_IMAGE_KERNEL_PATH=/kernel/vmlinux \
  -e SECONDBOX_CLIENT_IMAGE_KERNEL_CONFIG= \
  -e SECONDBOX_CLIENT_IMAGE_OUTPUT_REFERENCE=registry.example/secondbox/agent:local \
  -e SECONDBOX_CLIENT_IMAGE_PUBLIC_KEY=/signing/signing.pub \
  -e SECONDBOX_CLIENT_IMAGE_PUBLIC_KEY_SHA256=<64-lowercase-hex-fingerprint> \
  -e SECONDBOX_CLIENT_IMAGE_ROOTFS_SIZE_MIB=12288 \
  -e SECONDBOX_CLIENT_IMAGE_ROOTFS_SOURCE_DIR=/output/rootfs-source \
  -e SECONDBOX_CLIENT_IMAGE_ROOTFS_UUID=<fixed-uuid> \
  -e SECONDBOX_CLIENT_IMAGE_SHARED_FORMAT=ext4 \
  -e SECONDBOX_CLIENT_IMAGE_SHARED_SIZE_MIB=256 \
  -e SECONDBOX_CLIENT_IMAGE_SIGNING_KEY=/signing/signing.key \
  -e SECONDBOX_CLIENT_IMAGE_SOURCE_COMMIT="$(git rev-parse HEAD)" \
  -e SECONDBOX_CLIENT_IMAGE_SOURCE_REFERENCE=registry.example/base/userspace@sha256:<digest> \
  secondbox-client-image-builder:local

printf '%s' "$REGISTRY_PUSH_TOKEN" | docker login registry.example \
  --username "$REGISTRY_PUSH_USERNAME" --password-stdin
docker push registry.example/secondbox/agent:local
docker logout registry.example
```

Authenticate to the output registry before publication and push the exact reference supplied in `SECONDBOX_CLIENT_IMAGE_OUTPUT_REFERENCE`.
The signing private key stays outside the OCI image.
The Runner receives only the public key and its DER SHA-256 fingerprint.
The output image is a distribution artifact, not a normal Linux process image.
Firecracker Runners boot its kernel and rootfs; gVisor Runners use only its signed `rootfs.ext4` as the sandbox root with their own pinned guest agent.

## Release distribution and host materialization

The signed bundle is approximately 11 GB and is not embedded in the source-less GitHub release zip. Local release preparation reads an independently signed bundle from the absolute path configured by `SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR`, verifies it against `SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256`, and builds the exact allowlist as the dedicated `microvm-artifacts` OCI archive. The hosted publisher pushes that supplied archive without rebuilding it. Image labels and the artifact manifest bind the verified public-key fingerprint and `manifest.json` digest.

For a manual deployment with an installed bundle, materialize the release's
digest-pinned `microvm-artifacts` image into the operator-selected artifact
directory and verify it with the independent public key before enrolling the
Runner. Set `firecracker_installed_bundle = true`, `artifact_host_directory`,
and the matching trust and asset pins in the Runner declaration;
[deployment operations](deployment.md) documents that contract. A Runner that
boots only client-selected execution images sets
`firecracker_installed_bundle = false` and needs no materialized bundle.
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
