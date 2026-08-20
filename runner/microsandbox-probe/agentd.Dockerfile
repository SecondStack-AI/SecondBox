FROM docker.io/library/rust:alpine@sha256:3c38f3f82c2f3d73da3b38e18d279393a04cb43ddded0e35088a8c3324d40900 AS builder

RUN apk add --no-cache musl-dev

WORKDIR /build
COPY . .

RUN cargo build --locked --release --manifest-path crates/agentd/Cargo.toml

FROM scratch
COPY --from=builder /build/target/release/agentd /agentd
