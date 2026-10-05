# Host CLI binaries for consumers that build SecondBox from source instead of
# downloading release binaries. The build cross-compiles secondbox and
# secondbox-deploy on the build platform with the release flags of
# scripts/release-stage.sh and lays them out as
# /secondbox/bin/<os>-<arch>/<command> beside /secondbox/identity.json.
ARG RELEASE_VERSION
ARG SOURCE_COMMIT

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.25.12-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS builder

ARG RELEASE_VERSION
ARG SOURCE_COMMIT

RUN --mount=type=bind,source=scripts/require-image-build-identity.sh,target=/run/secondbox/require-image-build-identity.sh \
    sh /run/secondbox/require-image-build-identity.sh "${RELEASE_VERSION}" "${SOURCE_COMMIT}"

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN set -eu; \
    ldflags="-s -w -X github.com/SecondStack-AI/SecondBox/pkg/buildinfo.Version=${RELEASE_VERSION} -X github.com/SecondStack-AI/SecondBox/pkg/buildinfo.SourceCommit=${SOURCE_COMMIT}"; \
    for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
      os="${platform%/*}"; arch="${platform#*/}"; \
      for command in secondbox secondbox-deploy; do \
        CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false -ldflags "$ldflags" \
          -o "/out/secondbox/bin/$os-$arch/$command" "./cmd/$command"; \
      done; \
    done; \
    # The identity file is the stamped identity the build-platform binary reports.
    "/out/secondbox/bin/$(go env GOHOSTOS)-$(go env GOHOSTARCH)/secondbox-deploy" --output json version >/out/secondbox/identity.json; \
    printf '{"version":"%s","sourceCommit":"%s"}\n' "${RELEASE_VERSION}" "${SOURCE_COMMIT}" | cmp -s - /out/secondbox/identity.json \
      || { echo "SecondBox host CLI build: secondbox-deploy reports an identity other than the build arguments" >&2; exit 1; }

FROM scratch

ARG RELEASE_VERSION
ARG SOURCE_COMMIT

LABEL org.opencontainers.image.source="https://github.com/SecondStack-AI/SecondBox" \
      org.opencontainers.image.title="SecondBox host CLI binaries" \
      org.opencontainers.image.version="${RELEASE_VERSION}" \
      org.opencontainers.image.revision="${SOURCE_COMMIT}"

COPY --from=builder /out/secondbox /secondbox
COPY LICENSE /secondbox/LICENSE
