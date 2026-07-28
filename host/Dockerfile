FROM docker.io/library/golang:1.25.12-bookworm AS host-builder

WORKDIR /src/host
COPY apps/sandbox-service/host/go.mod apps/sandbox-service/host/go.sum ./
RUN go mod download && go mod verify
COPY apps/sandbox-service/host/ ./
COPY apps/agent-manager/agent/ /src/agent/
RUN cd /src \
    && HARNESS_SOURCE_SHA256="$(host/scripts/harness-source-sha256.sh)" \
    && test "${#HARNESS_SOURCE_SHA256}" -eq 64 \
    && cd /src/host \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
      -ldflags="-X secondstack/sandbox-host/internal/firecracker.harnessBuildID=${HARNESS_SOURCE_SHA256}" \
      -o /out/sandbox-host ./cmd/sandbox-host \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
      -o /out/sandbox-guest-agent ./cmd/sandbox-guest-agent

FROM docker.io/library/node:22.23.1-bookworm-slim AS harness-builder

WORKDIR /src/agent
COPY apps/agent-manager/agent/package.json apps/agent-manager/agent/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY apps/agent-manager/agent/ ./
RUN npm run build \
    && npm prune --omit=dev --no-audit \
    && npm cache clean --force \
    && install -m 0755 scripts/agent-harness dist/agent-harness

FROM docker.io/library/debian:12.12-slim AS firecracker-release

ARG FIRECRACKER_VERSION=1.16.1
ARG FIRECRACKER_ARCHIVE_SHA256=382a02a869e4d6d5cb14c40577f9545e8458021ea8b0b2d3fc10ec14d9c242e6

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && curl --fail --show-error --silent --location \
      "https://github.com/firecracker-microvm/firecracker/releases/download/v${FIRECRACKER_VERSION}/firecracker-v${FIRECRACKER_VERSION}-x86_64.tgz" \
      --output /tmp/firecracker.tgz \
    && echo "${FIRECRACKER_ARCHIVE_SHA256}  /tmp/firecracker.tgz" | sha256sum --check --strict \
    && mkdir /tmp/firecracker \
    && tar --extract --gzip --file /tmp/firecracker.tgz --directory /tmp/firecracker --strip-components=1 \
    && install -m 0755 "/tmp/firecracker/firecracker-v${FIRECRACKER_VERSION}-x86_64" /out-firecracker \
    && install -m 0755 "/tmp/firecracker/jailer-v${FIRECRACKER_VERSION}-x86_64" /out-jailer \
    && install -m 0644 /tmp/firecracker/LICENSE /out-LICENSE \
    && install -m 0644 /tmp/firecracker/NOTICE /out-NOTICE \
    && install -m 0644 /tmp/firecracker/THIRD-PARTY /out-THIRD-PARTY

FROM docker.io/library/node:22.23.1-bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      bubblewrap ca-certificates dbus e2fsprogs iproute2 iptables nftables procps systemd util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 sandbox-host \
    && useradd --system --uid 10001 --gid 10001 --home-dir /var/lib/sandbox-host --create-home --shell /usr/sbin/nologin sandbox-host \
    && ln -s /usr/local/bin/node /usr/bin/node \
    && rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
    && rm -f /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
    && install -d -m 0700 /run/sandbox-host /var/lib/sandbox-host

COPY --from=host-builder /out/sandbox-host /usr/local/bin/sandbox-host
COPY --from=host-builder /out/sandbox-guest-agent /usr/local/bin/sandbox-guest-agent
COPY --from=firecracker-release /out-firecracker /usr/local/bin/firecracker
COPY --from=firecracker-release /out-jailer /usr/local/bin/jailer
COPY --from=firecracker-release /out-LICENSE /usr/share/licenses/firecracker/LICENSE
COPY --from=firecracker-release /out-NOTICE /usr/share/licenses/firecracker/NOTICE
COPY --from=firecracker-release /out-THIRD-PARTY /usr/share/licenses/firecracker/THIRD-PARTY
COPY --from=harness-builder --chown=10001:10001 /src/agent/package.json /opt/agent-harness/package.json
COPY --from=harness-builder --chown=10001:10001 /src/agent/package-lock.json /opt/agent-harness/package-lock.json
COPY --from=harness-builder --chown=10001:10001 /src/agent/node_modules /opt/agent-harness/node_modules
COPY --from=harness-builder --chown=10001:10001 /src/agent/dist /opt/agent-harness/dist
COPY LICENSE /usr/share/licenses/sandbox-host/LICENSE
COPY --chmod=0755 apps/sandbox-service/host/scripts/container/sandbox-host-entrypoint.sh /usr/local/bin/sandbox-host-entrypoint
COPY --chmod=0755 apps/sandbox-service/host/scripts/microvm-host-network-setup.sh /usr/local/bin/microvm-host-network-setup
COPY apps/sandbox-service/host/deploy/secondstack-sandbox-host.service /etc/systemd/system/secondstack-sandbox-host.service

RUN ln -s /etc/systemd/system/secondstack-sandbox-host.service /etc/systemd/system/multi-user.target.wants/secondstack-sandbox-host.service

USER 0:0
WORKDIR /var/lib/sandbox-host
STOPSIGNAL SIGRTMIN+3
HEALTHCHECK --interval=10s --timeout=5s --start-period=15s --retries=4 CMD ["/usr/bin/setpriv", "--reuid=10001", "--regid=10001", "--clear-groups", "/usr/local/bin/sandbox-host", "-healthcheck", "-healthcheck-timeout", "5s"]
ENTRYPOINT ["/usr/local/bin/sandbox-host-entrypoint"]
CMD ["/lib/systemd/systemd"]
