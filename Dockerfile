FROM docker.io/library/golang:1.25.12-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/sandbox-service ./cmd/sandbox-service

FROM docker.io/library/debian:12.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/sandbox-service /usr/local/bin/sandbox-service
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/sandbox-service"]
