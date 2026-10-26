ARG RELEASE_VERSION
ARG SOURCE_COMMIT
ARG TARGETOS
ARG TARGETARCH
ARG PUBLIC_CONTRACT_DIGEST

FROM docker.io/library/golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS builder

ARG RELEASE_VERSION
ARG SOURCE_COMMIT
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X github.com/SecondStack-AI/SecondBox/pkg/buildinfo.Version=${RELEASE_VERSION} -X github.com/SecondStack-AI/SecondBox/pkg/buildinfo.SourceCommit=${SOURCE_COMMIT}" \
    -o /out/secondboxd ./cmd/secondboxd

FROM docker.io/library/debian:12.15-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8

ARG RELEASE_VERSION
ARG SOURCE_COMMIT
ARG PUBLIC_CONTRACT_DIGEST

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && install -d -o 65532 -g 65532 -m 0750 /var/lib/secondbox /var/log/secondbox \
    && rm -rf /var/lib/apt/lists/*
LABEL org.opencontainers.image.source="https://github.com/SecondStack-AI/SecondBox" \
      org.opencontainers.image.title="SecondBox control plane" \
      org.opencontainers.image.version="${RELEASE_VERSION}" \
      org.opencontainers.image.revision="${SOURCE_COMMIT}" \
      ai.secondstack.secondbox.public-contract-digest="${PUBLIC_CONTRACT_DIGEST}"

COPY --from=builder /out/secondboxd /usr/local/bin/secondboxd
COPY LICENSE /usr/share/licenses/secondbox/LICENSE
WORKDIR /var/lib/secondbox
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/secondboxd"]
