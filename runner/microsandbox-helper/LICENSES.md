# Local dependency inventory

The helper is built locally and is not published. Its locked direct dependencies are:

| Input | Pin | License |
| --- | --- | --- |
| Microsandbox source | `5b335537afad433ad2c0308cb54de13b7015b4e7` plus SecondBox patch `f38294823f2c8e3b8e7918a8c58b48b0c9c7c521874add5d5985af3d4134eb7c` | Apache-2.0 |
| `microsandbox-network` / `microsandbox-types` | 0.6.8 from the exact pinned local Microsandbox source tree | Apache-2.0 |
| `msb_krun` | 0.1.30, checksum `b57b2304dc1cef25b7cdd93be44c6515a97c90e00308b7d35eabc4fe27b02af5` | Apache-2.0 |
| `msb_krun_utils` | 0.1.30, checksum `5f4f682dec7289463f89adfd1df7605a425c069d238265496a99dbff921075a9` | Apache-2.0 |
| libkrunfw | `21cb6dce19a615f63e41ecb913334d18560c1364`, 5.6.1 | LGPL-2.1-only; bundled kernel and patches GPL-2.0-only |
| prost / prost-build | 0.14.3 | Apache-2.0 |
| thiserror | 2.0.17 | MIT OR Apache-2.0 |
| libc | 0.2.183 | MIT OR Apache-2.0 |

`Cargo.lock` is authoritative for the full transitive inventory. Production build evidence records
the exact patched tree, lock digest, helper binary digest, Rust toolchain, and host platform.
