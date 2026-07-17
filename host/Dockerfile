FROM golang:1.25.12-bookworm AS go-builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o /out/agentcy ./cmd/agentcy \
	&& CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o /out/agentcy-mcp-cutover ./cmd/agentcy-mcp-cutover \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o /out/agentcy-egress-proxy ./cmd/agentcy-egress-proxy \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o /out/agentcy-vmlauncher ./cmd/agentcy-vmlauncher

FROM node:22.23.1-bookworm-slim AS harness-builder

WORKDIR /src/agent

COPY agent/package.json agent/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY agent/ ./
RUN npm run build \
    && npm prune --omit=dev --no-audit \
    && npm cache clean --force \
    && install -m 0755 scripts/agent-harness dist/agent-harness

FROM node:22.23.1-bookworm-slim AS agent-service

RUN apt-get update \
    && apt-get install -y --no-install-recommends bubblewrap ca-certificates util-linux wget \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 agent-manager \
    && useradd --system --uid 10001 --gid 10001 --home-dir /var/lib/agent-manager --create-home --shell /usr/sbin/nologin agent-manager \
    && install -d -o 10001 -g 10001 -m 0750 /var/lib/agent-manager /var/log/agent-manager \
    && ln -s /usr/local/bin/node /usr/bin/node \
    && rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
    && rm -f /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack

COPY --from=go-builder /out/agentcy /usr/local/bin/agentcy
COPY --from=go-builder /out/agentcy-mcp-cutover /usr/local/bin/agentcy-mcp-cutover
COPY --from=harness-builder --chown=10001:10001 /src/agent/package.json /opt/agent-harness/package.json
COPY --from=harness-builder --chown=10001:10001 /src/agent/package-lock.json /opt/agent-harness/package-lock.json
COPY --from=harness-builder --chown=10001:10001 /src/agent/node_modules /opt/agent-harness/node_modules
COPY --from=harness-builder --chown=10001:10001 /src/agent/dist /opt/agent-harness/dist
COPY LICENSE /usr/share/licenses/agent-manager/LICENSE
COPY --chmod=0755 scripts/container/agent-service-entrypoint.sh /usr/local/bin/agent-service-entrypoint

USER 0:0
WORKDIR /var/lib/agent-manager
EXPOSE 8081
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=4 CMD ["wget", "--quiet", "--output-document=-", "http://127.0.0.1:8081/api/health"]
ENTRYPOINT ["/usr/local/bin/agent-service-entrypoint"]

FROM debian:12.12-slim AS agent-egress-proxy

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10002 agent-egress \
    && useradd --system --uid 10002 --gid 10002 --home-dir /var/lib/agent-egress --create-home --shell /usr/sbin/nologin agent-egress \
    && install -d -o 10002 -g 10002 -m 0750 /var/lib/agent-egress /run/agent-egress

COPY --from=go-builder /out/agentcy-egress-proxy /usr/local/bin/agentcy-egress-proxy
COPY LICENSE /usr/share/licenses/agent-manager/LICENSE
COPY --chmod=0755 scripts/container/egress-proxy-entrypoint.sh /usr/local/bin/egress-proxy-entrypoint

USER 0:0
WORKDIR /var/lib/agent-egress
EXPOSE 18080 18099
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=4 CMD ["/usr/local/bin/agentcy-egress-proxy", "-healthcheck", "-healthcheck-url", "http://127.0.0.1:18099", "-healthcheck-timeout", "5s"]
ENTRYPOINT ["/usr/local/bin/egress-proxy-entrypoint"]

FROM debian:12.12-slim AS firecracker-release

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

FROM node:22.23.1-bookworm-slim AS agent-sandbox-host

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      bubblewrap \
      ca-certificates \
      dbus \
      e2fsprogs \
      iproute2 \
      iptables \
      nftables \
      procps \
      systemd \
      util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 agent-manager \
    && useradd --system --uid 10001 --gid 10001 --home-dir /var/lib/agent-manager --create-home --shell /usr/sbin/nologin agent-manager \
    && ln -s /usr/local/bin/node /usr/bin/node \
    && rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
    && rm -f /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
    && install -d -m 0700 /run/agent-sandbox-host /var/lib/agent-sandbox-host

COPY --from=go-builder /out/agentcy-vmlauncher /usr/local/bin/agentcy-vmlauncher
COPY --from=firecracker-release /out-firecracker /usr/local/bin/firecracker
COPY --from=firecracker-release /out-jailer /usr/local/bin/jailer
COPY --from=firecracker-release /out-LICENSE /usr/share/licenses/firecracker/LICENSE
COPY --from=firecracker-release /out-NOTICE /usr/share/licenses/firecracker/NOTICE
COPY --from=firecracker-release /out-THIRD-PARTY /usr/share/licenses/firecracker/THIRD-PARTY
COPY --from=harness-builder --chown=10001:10001 /src/agent/package.json /opt/agent-harness/package.json
COPY --from=harness-builder --chown=10001:10001 /src/agent/package-lock.json /opt/agent-harness/package-lock.json
COPY --from=harness-builder --chown=10001:10001 /src/agent/node_modules /opt/agent-harness/node_modules
COPY --from=harness-builder --chown=10001:10001 /src/agent/dist /opt/agent-harness/dist
COPY LICENSE /usr/share/licenses/agent-manager/LICENSE
COPY --chmod=0755 scripts/container/sandbox-host-entrypoint.sh /usr/local/bin/sandbox-host-entrypoint
COPY --chmod=0755 scripts/microvm-host-network-setup.sh /usr/local/bin/microvm-host-network-setup
COPY deploy/secondstack-agent-sandbox-host.service /etc/systemd/system/secondstack-agent-sandbox-host.service

RUN ln -s /etc/systemd/system/secondstack-agent-sandbox-host.service /etc/systemd/system/multi-user.target.wants/secondstack-agent-sandbox-host.service

USER 0:0
WORKDIR /var/lib/agent-sandbox-host
STOPSIGNAL SIGRTMIN+3
HEALTHCHECK --interval=10s --timeout=5s --start-period=15s --retries=4 CMD ["/usr/bin/setpriv", "--reuid=10001", "--regid=10001", "--clear-groups", "/usr/local/bin/agentcy-vmlauncher", "-healthcheck", "-healthcheck-timeout", "5s"]
ENTRYPOINT ["/usr/local/bin/sandbox-host-entrypoint"]
CMD ["/lib/systemd/systemd"]

FROM node:22.23.1-bookworm-slim AS development

COPY --from=go-builder /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 agent-manager \
    && useradd --system --uid 10001 --gid 10001 --home-dir /home/agent-manager --create-home --shell /bin/sh agent-manager \
    && install -d -o 10001 -g 10001 -m 0750 /workspace /var/lib/agent-manager

COPY --from=harness-builder --chown=10001:10001 /src/agent /opt/agent-harness
COPY --chown=10001:10001 . /workspace
COPY --chmod=0755 scripts/container/agent-manager-dev.sh /usr/local/bin/agent-manager-dev

USER 10001:10001
WORKDIR /workspace
EXPOSE 8081
HEALTHCHECK --interval=15s --timeout=5s --start-period=45s --retries=4 CMD ["node", "-e", "fetch('http://127.0.0.1:8081/api/health').then(r=>{if(!r.ok)process.exit(1)}).catch(()=>process.exit(1))"]
ENTRYPOINT ["/usr/local/bin/agent-manager-dev"]
