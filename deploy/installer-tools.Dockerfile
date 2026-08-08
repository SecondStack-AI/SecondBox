FROM docker.io/library/debian:12.12-slim@sha256:d5d3f9c23164ea16f31852f95bd5959aad1c5e854332fe00f7b3a20fcc9f635c

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
ENTRYPOINT ["/usr/bin/mkfs.btrfs"]
