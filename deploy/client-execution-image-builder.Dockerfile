FROM docker.io/library/golang:1.25.12-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      docker.io \
      e2fsprogs \
      fakeroot \
      jq \
      openssl \
      python3 \
    && rm -rf /var/lib/apt/lists/*

COPY . /src
WORKDIR /src

ENTRYPOINT ["/src/scripts/build-client-execution-image.sh"]
