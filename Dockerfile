FROM docker.io/library/golang:1.25.12-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/secondboxd ./cmd/secondboxd

FROM docker.io/library/debian:12.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && install -d -o 65532 -g 65532 -m 0750 /var/lib/secondbox /var/log/secondbox \
    && rm -rf /var/lib/apt/lists/*
LABEL org.opencontainers.image.source="https://github.com/SecondStack-AI/SecondBox" \
      org.opencontainers.image.title="SecondBox control plane"

COPY --from=builder /out/secondboxd /usr/local/bin/secondboxd
COPY LICENSE /usr/share/licenses/secondbox/LICENSE
WORKDIR /var/lib/secondbox
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/secondboxd"]
