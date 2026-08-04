# Release operator setup

SecondBox releases are built locally and published by one GitHub workflow. There is no candidate, qualification, attestation, or finalization phase.

## One-time setup

- Authenticate the local `gh` CLI for `SecondStack-AI/SecondBox`.
- Configure npm trusted publishing for `@secondstack-ai/secondbox` and `.github/workflows/release.yml`.
- Allow that workflow `contents: write`, `packages: write`, and `id-token: write`.
- Give the repository workflow access to the public `control-plane`, `runner`, and `microvm-artifacts` GHCR packages.

The local build host needs Docker Buildx, `jq`, `openssl`, npm, Node.js, Go, and `just`. Release staging also needs the microVM release bundle variables documented by `scripts/release-stage.sh` because the runner consumes that bundle at runtime.

## Release

Tag the commit, build the artifacts, and upload them:

```sh
git tag v0.1.5
git push origin refs/tags/v0.1.5

just release-stage 0.1.5 /protected/releases/secondbox-0.1.5
just release-upload 0.1.5 /protected/releases/secondbox-0.1.5
```

`release-upload` creates a private draft, uploads the local output, and dispatches the GitHub workflow. The workflow does not rebuild anything. It publishes the three OCI archives to GHCR, publishes the TypeScript package directly under npm `latest`, removes the transport-only archives from the draft, and opens a stable GitHub release marked latest.

Watch the dispatched run with:

```sh
gh run list --workflow release.yml --limit 1
gh run watch --exit-status
```

If publication fails, fix the cause and run `just release-upload` again while the release is still a draft. Never move a published tag; use a new patch version instead.
