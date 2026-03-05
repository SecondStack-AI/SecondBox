FROM golang:1.23-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o agentcy ./cmd/agentcy

FROM alpine:latest
RUN apk add --no-cache ca-certificates docker-cli
WORKDIR /app
COPY --from=builder /build/agentcy .
COPY --from=builder /build/cmd/agentcy/public ./public

EXPOSE 8081
ENTRYPOINT ["./agentcy"]
