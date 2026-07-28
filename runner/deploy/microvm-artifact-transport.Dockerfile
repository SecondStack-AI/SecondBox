FROM scratch

# This image is a release transport for the independently signed host bundle.
# Keep the allowlist explicit so build-context files can never enter the image.
COPY kernel /secondbox-runner-microvm/kernel
COPY rootfs.ext4 /secondbox-runner-microvm/rootfs.ext4
COPY shared.img /secondbox-runner-microvm/shared.img
COPY kernel-provenance.json /secondbox-runner-microvm/kernel-provenance.json
COPY rootfs-source-manifest.json /secondbox-runner-microvm/rootfs-source-manifest.json
COPY secondbox-rootfs-contract.json /secondbox-runner-microvm/secondbox-rootfs-contract.json
COPY rootfs-debian-packages.lock /secondbox-runner-microvm/rootfs-debian-packages.lock
COPY rootfs-python.freeze /secondbox-runner-microvm/rootfs-python.freeze
COPY rootfs-debian-license-inventory.json /secondbox-runner-microvm/rootfs-debian-license-inventory.json
COPY rootfs-python-license-inventory.json /secondbox-runner-microvm/rootfs-python-license-inventory.json
COPY runtime-manifest.json /secondbox-runner-microvm/runtime-manifest.json
COPY toolchain-manifest.json /secondbox-runner-microvm/toolchain-manifest.json
COPY manifest.json /secondbox-runner-microvm/manifest.json
COPY manifest.sig /secondbox-runner-microvm/manifest.sig
COPY signing.pub /secondbox-runner-microvm/signing.pub
COPY SHA256SUMS /secondbox-runner-microvm/SHA256SUMS
