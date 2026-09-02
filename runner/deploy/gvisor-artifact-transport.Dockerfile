# The gVisor artifact transport: the flat root prepared from the reviewed
# source image, the pinned runsc and the guest agent as launch artifacts, and
# the backend materialization whose digest a runner and the SecondStack
# operator pin. Everything is derived from this repository and the two
# digest-pinned bases, so a release needs no operator-built inputs.
ARG RELEASE_VERSION
ARG SOURCE_COMMIT

FROM docker.io/library/alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce AS flat-root-source

FROM docker.io/library/golang:1.25.12-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS tools

WORKDIR /src/runner
COPY runner/go.mod runner/go.sum ./
RUN go mod download && go mod verify
COPY runner/ ./
RUN for tool in secondbox-guest-agent secondbox-prepare-gvisor-flat-root secondbox-flat-root-digest secondbox-materialization-digest; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o "/out/$tool" "./cmd/$tool" || exit 1; \
    done

FROM docker.io/library/debian:12.12-slim@sha256:d5d3f9c23164ea16f31852f95bd5959aad1c5e854332fe00f7b3a20fcc9f635c AS runsc-release

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY --chmod=0755 runner/scripts/fetch-runsc.sh /usr/local/bin/fetch-runsc
RUN /usr/local/bin/fetch-runsc --output /out

FROM docker.io/library/debian:12.12-slim@sha256:d5d3f9c23164ea16f31852f95bd5959aad1c5e854332fe00f7b3a20fcc9f635c AS assemble

ARG RELEASE_VERSION
RUN apt-get update \
    && apt-get install -y --no-install-recommends jq \
    && rm -rf /var/lib/apt/lists/*
COPY --from=flat-root-source / /secondbox-runner-gvisor/rootfs
COPY --from=tools /out/secondbox-guest-agent /secondbox-runner-gvisor/bin/secondbox-guest-agent
COPY --from=runsc-release /out/runsc /secondbox-runner-gvisor/bin/runsc
# The verifiers an operator runs after extraction, at the release's version.
COPY --from=tools /out/secondbox-flat-root-digest /out/secondbox-materialization-digest /secondbox-runner-gvisor/bin/
COPY --from=tools /out/secondbox-prepare-gvisor-flat-root /out/secondbox-flat-root-digest /out/secondbox-materialization-digest /usr/local/bin/
COPY runner/scripts/fetch-runsc.sh /tmp/fetch-runsc.sh
RUN set -eu; \
    root=/secondbox-runner-gvisor; \
    runsc_release="$(sed -n 's/^readonly RUNSC_RELEASE="\([0-9.]*\)"$/\1/p' /tmp/fetch-runsc.sh)"; \
    test -n "$runsc_release"; \
    digest_text() { printf '%s' "$1" | sha256sum | awk '{print "sha256:" $1}'; }; \
    digest_file() { sha256sum "$1" | awk '{print "sha256:" $1}'; }; \
    secondbox-prepare-gvisor-flat-root "$root/rootfs"; \
    # Image layers carry whole-second timestamps; digest the flat root as it\
    # will exist after extraction on a host.\
    find "$root/rootfs" -exec sh -c 'for entry; do touch -h -d "@$(stat -c %Y "$entry")" "$entry"; done' _ {} +; \
    flat_root_digest="$(secondbox-flat-root-digest "$root/rootfs")"; \
    jq -cn \
      --arg runtime "$(digest_text "secondbox-gvisor-runtime-release-$runsc_release-linux-amd64")" \
      --arg toolchain "$(digest_text "secondbox-gvisor-toolchain-release-$runsc_release-linux-amd64")" \
      --arg source "$(digest_text 'alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce')" \
      --arg flatRoot "$flat_root_digest" \
      --arg agent "$(digest_file "$root/bin/secondbox-guest-agent")" \
      --arg runsc "$(digest_file "$root/bin/runsc")" \
      --arg build "secondbox-gvisor-release-${RELEASE_VERSION}" \
      --arg helperBuild "runsc-release-$runsc_release" \
      '{schemaVersion:"secondbox.runner/backend-materialization/v1",key:{backendKind:"gvisor",guestArchitecture:"amd64",runtimeManifestDigest:$runtime,toolchainManifestDigest:$toolchain},sourceOciManifestDigest:$source,flatRootDigest:$flatRoot,launchArtifacts:[{id:"guest-agent",sha256:$agent},{id:"runsc",sha256:$runsc}],agentProtocolGeneration:1,agentFeatures:["exec-streaming","file-streaming","port-proxy","pty"],backendBuildId:$build,helperBuildId:$helperBuild}' \
      >"$root/materialization.json"; \
    materialization_digest="$(secondbox-materialization-digest "$root/materialization.json")"; \
    jq -cn --arg materialization "$materialization_digest" --arg flatRoot "$flat_root_digest" --arg runsc "$runsc_release" \
      '{materializationDigest:$materialization,flatRootDigest:$flatRoot,runscRelease:$runsc}' >"$root/identity.json"; \
    (cd "$root" && sha256sum bin/runsc bin/secondbox-guest-agent bin/secondbox-flat-root-digest bin/secondbox-materialization-digest materialization.json identity.json >SHA256SUMS)

# `--target metadata` exports the identity the release manifest records.
FROM scratch AS metadata
COPY --from=assemble /secondbox-runner-gvisor/materialization.json /materialization.json
COPY --from=assemble /secondbox-runner-gvisor/identity.json /identity.json

FROM scratch

ARG RELEASE_VERSION
ARG SOURCE_COMMIT

LABEL org.opencontainers.image.source="https://github.com/SecondStack-AI/SecondBox" \
      org.opencontainers.image.title="SecondBox gVisor artifacts" \
      org.opencontainers.image.version="${RELEASE_VERSION}" \
      org.opencontainers.image.revision="${SOURCE_COMMIT}"

COPY --from=assemble /secondbox-runner-gvisor /secondbox-runner-gvisor
