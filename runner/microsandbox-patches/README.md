# Local Microsandbox dependency patch

SecondBox evaluates Microsandbox `v0.6.8` at commit
`5b335537afad433ad2c0308cb54de13b7015b4e7`. The evaluated source does not
expose the descriptor-safe ext4 UUID operations required by the backend.

This directory is the complete SecondBox-owned patch input. It is built only
from an operator-supplied local checkout by `runner/scripts/build-microsandbox-local.sh`.
The builder verifies the source commit and tree, rejects dirty input, verifies
the Cargo lock and patch digests, clones locally without hardlinks, applies the
patch in an isolated staging directory, verifies the resulting tree, and builds
the image library, `msb`, and probe with Cargo's locked offline mode. The caller's
source checkout must also contain the exact initialized `libkrunfw` submodule.
The builder compiles `libkrunfw` locally from its checksum-pinned Linux tarball,
builds static `agentd` with the evaluated Cargo lock in a digest-pinned container,
and exports a digest-pinned Alpine fixture. Runtime binaries, firmware, the rootfs,
and bounded build evidence are materialized only beneath the new output directory.

The patch is not published to an external fork or contribution branch. The
manifest records provenance, exact checksums, and the licenses of the pinned
dependency inputs.

Example:

```console
runner/scripts/build-microsandbox-local.sh \
  --source /path/to/clean/microsandbox \
  --output /tmp/secondbox-microsandbox-build
```

Both paths are mandatory. The output must not already exist. The command performs
no push, publication, or external repository write.
