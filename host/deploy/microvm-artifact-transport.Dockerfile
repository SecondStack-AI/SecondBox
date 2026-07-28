FROM scratch

# This image is a release transport for the independently signed host bundle.
# Keep the allowlist explicit so build-context files can never enter the image.
COPY kernel /sandbox-host-microvm/kernel
COPY rootfs.ext4 /sandbox-host-microvm/rootfs.ext4
COPY shared.img /sandbox-host-microvm/shared.img
COPY kernel-provenance.json /sandbox-host-microvm/kernel-provenance.json
COPY rootfs-source-manifest.json /sandbox-host-microvm/rootfs-source-manifest.json
COPY standard-toolset.json /sandbox-host-microvm/standard-toolset.json
COPY manifest.json /sandbox-host-microvm/manifest.json
COPY manifest.sig /sandbox-host-microvm/manifest.sig
COPY signing.pub /sandbox-host-microvm/signing.pub
COPY SHA256SUMS /sandbox-host-microvm/SHA256SUMS
