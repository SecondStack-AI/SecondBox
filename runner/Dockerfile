ARG RELEASE_VERSION
ARG SOURCE_COMMIT
ARG PUBLIC_CONTRACT_DIGEST

FROM docker.io/library/golang:1.25.12-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS runner-builder

ARG RELEASE_VERSION
ARG SOURCE_COMMIT
ARG TARGETARCH

RUN --mount=type=bind,source=scripts/require-image-build-identity.sh,target=/run/secondbox/require-image-build-identity.sh \
    sh /run/secondbox/require-image-build-identity.sh "${RELEASE_VERSION}" "${SOURCE_COMMIT}"

WORKDIR /src/runner
COPY runner/go.mod runner/go.sum ./
RUN go mod download && go mod verify
COPY runner/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false \
      -ldflags="-X main.releaseVersion=${RELEASE_VERSION} -X main.sourceCommit=${SOURCE_COMMIT}" \
      -o /out/secondbox-runner ./cmd/secondbox-runner \
    && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false \
      -o /out/secondbox-guest-agent ./cmd/secondbox-guest-agent \
    && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false \
      -o /out/secondbox-image-fetcher ./cmd/secondbox-image-fetcher

FROM docker.io/library/debian:12.12-slim@sha256:d5d3f9c23164ea16f31852f95bd5959aad1c5e854332fe00f7b3a20fcc9f635c AS firecracker-release

ARG TARGETARCH
ARG FIRECRACKER_VERSION=1.16.1
# Official release asset digests for each supported Runner architecture.
ARG FIRECRACKER_ARCHIVE_SHA256_X86_64=382a02a869e4d6d5cb14c40577f9545e8458021ea8b0b2d3fc10ec14d9c242e6
ARG FIRECRACKER_ARCHIVE_SHA256_AARCH64=8d0e69f6d6f9a1724551f607f18504052c16c1828ee3d4d7b6e6c73380871e0e

RUN case "${TARGETARCH}" in \
      amd64) firecracker_arch=x86_64; firecracker_sha256="${FIRECRACKER_ARCHIVE_SHA256_X86_64}" ;; \
      arm64) firecracker_arch=aarch64; firecracker_sha256="${FIRECRACKER_ARCHIVE_SHA256_AARCH64}" ;; \
      *) echo "SecondBox Runner image does not support architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && curl --fail --show-error --silent --location \
      "https://github.com/firecracker-microvm/firecracker/releases/download/v${FIRECRACKER_VERSION}/firecracker-v${FIRECRACKER_VERSION}-${firecracker_arch}.tgz" \
      --output /tmp/firecracker.tgz \
    && echo "${firecracker_sha256}  /tmp/firecracker.tgz" | sha256sum --check --strict \
    && mkdir /tmp/firecracker \
    && tar --extract --gzip --file /tmp/firecracker.tgz --directory /tmp/firecracker --strip-components=1 \
    && install -m 0755 "/tmp/firecracker/firecracker-v${FIRECRACKER_VERSION}-${firecracker_arch}" /out-firecracker \
    && install -m 0755 "/tmp/firecracker/jailer-v${FIRECRACKER_VERSION}-${firecracker_arch}" /out-jailer \
    && install -m 0644 /tmp/firecracker/LICENSE /out-LICENSE \
    && install -m 0644 /tmp/firecracker/NOTICE /out-NOTICE \
    && install -m 0644 /tmp/firecracker/THIRD-PARTY /out-THIRD-PARTY

FROM docker.io/library/debian:12.12-slim@sha256:d5d3f9c23164ea16f31852f95bd5959aad1c5e854332fe00f7b3a20fcc9f635c

ARG RELEASE_VERSION
ARG SOURCE_COMMIT
ARG PUBLIC_CONTRACT_DIGEST

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      bubblewrap ca-certificates e2fsprogs iproute2 iptables nftables procps skopeo util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 secondbox-runner \
    && useradd --system --uid 10001 --gid 10001 --home-dir /var/lib/secondbox-runner --create-home --shell /usr/sbin/nologin secondbox-runner \
    && install -d -m 0700 /var/lib/secondbox-runner

LABEL org.opencontainers.image.source="https://github.com/SecondStack-AI/SecondBox" \
      org.opencontainers.image.title="SecondBox Firecracker runner" \
      org.opencontainers.image.version="${RELEASE_VERSION}" \
      org.opencontainers.image.revision="${SOURCE_COMMIT}" \
      ai.secondstack.secondbox.public-contract-digest="${PUBLIC_CONTRACT_DIGEST}"

COPY --from=runner-builder /out/secondbox-runner /usr/local/bin/secondbox-runner
COPY --from=runner-builder /out/secondbox-image-fetcher /usr/local/bin/secondbox-image-fetcher
COPY --from=runner-builder /out/secondbox-guest-agent /usr/local/bin/secondbox-guest-agent
COPY --from=firecracker-release /out-firecracker /usr/local/bin/firecracker
COPY --from=firecracker-release /out-jailer /usr/local/bin/jailer
COPY --from=firecracker-release /out-LICENSE /usr/share/licenses/firecracker/LICENSE
COPY --from=firecracker-release /out-NOTICE /usr/share/licenses/firecracker/NOTICE
COPY --from=firecracker-release /out-THIRD-PARTY /usr/share/licenses/firecracker/THIRD-PARTY
COPY LICENSE /usr/share/licenses/secondbox-runner/LICENSE
COPY runner/scripts/container/secondbox-runner-entrypoint.sh /usr/local/bin/secondbox-runner-entrypoint
COPY runner/scripts/microvm-host-network-setup.sh /usr/local/bin/microvm-host-network-setup
RUN chmod 0755 /usr/local/bin/secondbox-runner-entrypoint /usr/local/bin/microvm-host-network-setup

USER 0:0
WORKDIR /var/lib/secondbox-runner
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=10s --timeout=5s --start-period=15s --retries=4 CMD ["/usr/bin/setpriv", "--reuid=10001", "--regid=10001", "--clear-groups", "/usr/local/bin/secondbox-runner", "-healthcheck", "-healthcheck-timeout", "5s"]
ENTRYPOINT ["/usr/local/bin/secondbox-runner-entrypoint"]
CMD ["/usr/local/bin/secondbox-runner"]
