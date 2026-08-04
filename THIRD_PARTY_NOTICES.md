# Third-Party Notices

SecondBox includes source code that depends on third-party Go modules and builds execution assets from third-party projects. Those projects remain governed by their own licenses.

## Go modules

- `github.com/aws/aws-sdk-go-v2` and its `aws`, `credentials`, `service/s3`, and internal modules — Apache-2.0
- `github.com/aws/smithy-go` — Apache-2.0
- `github.com/gorilla/websocket` — BSD-2-Clause
- `github.com/jackc/pgx/v5` — MIT
- `github.com/jackc/pgpassfile` — MIT
- `github.com/jackc/pgservicefile` — MIT
- `github.com/jackc/puddle/v2` — MIT
- `golang.org/x/net` — BSD-3-Clause
- `golang.org/x/sync` — BSD-3-Clause
- `golang.org/x/sys` — BSD-3-Clause
- `golang.org/x/text` — BSD-3-Clause
- `google.golang.org/genproto/googleapis/rpc` — Apache-2.0
- `google.golang.org/grpc` — Apache-2.0
- `google.golang.org/protobuf` — BSD-3-Clause

## Contract tooling

- `typescript` and its platform packages — Apache-2.0
- `@types/node` — MIT
- `ajv` — MIT
- `@flue/runtime` — Apache-2.0 (peer dependency of the TypeScript `/flue` export)
- `ws` — MIT (runtime dependency of the TypeScript `/node` export)
- `@types/ws` — MIT

## Execution assets

- RustFS — Apache-2.0; the optional development Compose profile references the
  `rustfs/rustfs` image, with source at https://github.com/rustfs/rustfs
- Firecracker — Apache-2.0
- Linux kernel — GPL-2.0-only WITH Linux-syscall-note
- Debian packages used to construct the guest root filesystem — licenses recorded by the Debian package metadata copied into each generated image bundle
- OCI source images supplied to the image pipeline — governed by the source image's license; operators must approve the source before ingestion

SecondBox does not distribute a kernel, guest root filesystem, Firecracker binary, or OCI source image in this source repository. Release bundles must include the corresponding license texts and machine-readable provenance for every bundled asset.
