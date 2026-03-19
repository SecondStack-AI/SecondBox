FROM node:22-alpine AS frontend-builder

WORKDIR /build/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.25.5-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend-builder /build/cmd/agentcy/public ./cmd/agentcy/public
RUN CGO_ENABLED=0 go build -o agentcy ./cmd/agentcy

FROM alpine:3.21

RUN apk add --no-cache ca-certificates docker-cli
WORKDIR /app
COPY --from=builder /build/agentcy .
COPY --from=builder /build/cmd/agentcy/public ./public

EXPOSE 8081
ENTRYPOINT ["./agentcy"]
