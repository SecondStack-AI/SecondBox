# Builds the pinned Firecracker guest kernel for TARGETARCH with build-kernel.sh.
# The build stage runs on BUILDPLATFORM and cross-compiles, so no emulation is needed.
# Build context: runner/scripts/microvm-image. Output: /kernel holds exactly what
# build-kernel.sh writes.

# The source download depends only on kernel.lock and is shared by both target architectures.
FROM --platform=$BUILDPLATFORM docker.io/library/debian:12.12-slim@sha256:d5d3f9c23164ea16f31852f95bd5959aad1c5e854332fe00f7b3a20fcc9f635c AS kernel-source

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY kernel.lock /src/kernel.lock
RUN set -eu; \
    . /src/kernel.lock; \
    mkdir -p /cache; \
    curl -fsSL "$KERNEL_URL" -o "/cache/linux-$KERNEL_VERSION.tar.xz"; \
    printf '%s  %s\n' "$KERNEL_SHA256" "/cache/linux-$KERNEL_VERSION.tar.xz" | sha256sum -c -

FROM --platform=$BUILDPLATFORM docker.io/library/debian:12.12-slim@sha256:d5d3f9c23164ea16f31852f95bd5959aad1c5e854332fe00f7b3a20fcc9f635c AS kernel-build

ARG BUILDARCH
ARG TARGETARCH

# Every build uses the triplet-prefixed compiler, so native and cross builds
# record the same compiler identity in the kernel.
RUN set -eu; \
    case "$TARGETARCH" in \
        amd64) triplet=x86_64-linux-gnu; toolchain="gcc-x86-64-linux-gnu binutils-x86-64-linux-gnu" ;; \
        arm64) triplet=aarch64-linux-gnu; toolchain="gcc-aarch64-linux-gnu binutils-aarch64-linux-gnu" ;; \
        *) echo "unsupported TARGETARCH '$TARGETARCH'; the guest kernel is amd64 or arm64" >&2; exit 2 ;; \
    esac; \
    if [ "$BUILDARCH" = "$TARGETARCH" ]; then toolchain=; fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        bc bison ca-certificates curl flex gcc libc6-dev libelf-dev libssl-dev make xz-utils $toolchain; \
    rm -rf /var/lib/apt/lists/*; \
    echo "$triplet-" > /etc/secondbox-kernel-cross-compile

WORKDIR /src
COPY --from=kernel-source /cache /cache
COPY kernel.lock build-kernel.sh check-kernel-config.sh kernel-required.config kernel-required-amd64.config kernel-required-arm64.config ./
RUN SECONDBOX_RUNNER_MICROVM_ARCHITECTURE="$TARGETARCH" \
    SECONDBOX_RUNNER_MICROVM_KERNEL_LOCK=/src/kernel.lock \
    SECONDBOX_RUNNER_MICROVM_KERNEL_CACHE=/cache \
    SECONDBOX_RUNNER_MICROVM_KERNEL_JOBS="$(nproc)" \
    SECONDBOX_RUNNER_MICROVM_KERNEL_VERIFY_ONLY=false \
    SECONDBOX_RUNNER_MICROVM_KERNEL_CROSS_COMPILE="$(cat /etc/secondbox-kernel-cross-compile)" \
    ./build-kernel.sh /out

FROM scratch
COPY --from=kernel-build /out/ /kernel/
