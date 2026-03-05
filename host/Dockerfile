FROM golang:1.23-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o agent-manager ./cmd/agent-manager

FROM alpine:latest
RUN apk add --no-cache ca-certificates docker-cli
WORKDIR /app
COPY --from=builder /build/agent-manager .
COPY --from=builder /build/cmd/agent-manager/public ./public

EXPOSE 8081
ENTRYPOINT ["./agent-manager"]
