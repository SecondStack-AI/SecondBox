FROM docker.io/library/debian:12.15-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8

ARG RELEASE_VERSION
ARG SOURCE_COMMIT

RUN apt-get update \
    && apt-get install -y --no-install-recommends btrfs-progs \
    && rm -rf /var/lib/apt/lists/*

LABEL org.opencontainers.image.source="https://github.com/SecondStack-AI/SecondBox" \
      org.opencontainers.image.title="SecondBox installer filesystem tools" \
      org.opencontainers.image.version="${RELEASE_VERSION}" \
      org.opencontainers.image.revision="${SOURCE_COMMIT}"

USER 0:0
ENTRYPOINT ["/usr/sbin/mkfs.btrfs"]
