FROM docker.io/library/golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      docker.io \
      e2fsprogs \
      fakeroot \
      jq \
      python3 \
    && rm -rf /var/lib/apt/lists/*

COPY . /src
WORKDIR /src

ENTRYPOINT ["/src/scripts/build-client-execution-image.sh"]
