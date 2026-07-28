FROM docker.io/library/golang:1.25.12-bookworm AS runner-builder

WORKDIR /src/runner
COPY runner/go.mod runner/go.sum ./
RUN go mod download && go mod verify
COPY runner/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
      -o /out/secondbox-runner ./cmd/secondbox-runner \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
      -o /out/secondbox-guest-agent ./cmd/secondbox-guest-agent

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

FROM docker.io/library/debian:12.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      bubblewrap ca-certificates dbus e2fsprogs iproute2 iptables nftables procps systemd util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 secondbox-runner \
    && useradd --system --uid 10001 --gid 10001 --home-dir /var/lib/secondbox-runner --create-home --shell /usr/sbin/nologin secondbox-runner \
    && install -d -m 0700 /run/secondbox-runner /var/lib/secondbox-runner

LABEL org.opencontainers.image.source="https://github.com/SecondStack-AI/SecondBox" \
      org.opencontainers.image.title="SecondBox Firecracker runner"

COPY --from=runner-builder /out/secondbox-runner /usr/local/bin/secondbox-runner
COPY --from=runner-builder /out/secondbox-guest-agent /usr/local/bin/secondbox-guest-agent
COPY --from=firecracker-release /out-firecracker /usr/local/bin/firecracker
COPY --from=firecracker-release /out-jailer /usr/local/bin/jailer
COPY --from=firecracker-release /out-LICENSE /usr/share/licenses/firecracker/LICENSE
COPY --from=firecracker-release /out-NOTICE /usr/share/licenses/firecracker/NOTICE
COPY --from=firecracker-release /out-THIRD-PARTY /usr/share/licenses/firecracker/THIRD-PARTY
COPY LICENSE /usr/share/licenses/secondbox-runner/LICENSE
COPY --chmod=0755 runner/scripts/container/secondbox-runner-entrypoint.sh /usr/local/bin/secondbox-runner-entrypoint
COPY --chmod=0755 runner/scripts/microvm-host-network-setup.sh /usr/local/bin/microvm-host-network-setup
COPY runner/deploy/secondbox-runner.service /etc/systemd/system/secondbox-runner.service

RUN ln -s /etc/systemd/system/secondbox-runner.service /etc/systemd/system/multi-user.target.wants/secondbox-runner.service

USER 0:0
WORKDIR /var/lib/secondbox-runner
STOPSIGNAL SIGRTMIN+3
HEALTHCHECK --interval=10s --timeout=5s --start-period=15s --retries=4 CMD ["/usr/bin/setpriv", "--reuid=10001", "--regid=10001", "--clear-groups", "/usr/local/bin/secondbox-runner", "-healthcheck", "-healthcheck-timeout", "5s"]
ENTRYPOINT ["/usr/local/bin/secondbox-runner-entrypoint"]
CMD ["/lib/systemd/systemd"]
