# Downstream release integration

The first coordinated release version is `0.1.0`. Do not integrate it until the public final release index exists and `secondbox-deploy verify release-index` accepts it. The index, rather than the Git tag or a registry listing, is release authority.

Download the `secondbox-deploy_0.1.0_OS_ARCH` binary and verify its checksum from the public `SHA256SUMS`. On Linux amd64, verify the complete authority chain:

```text
secondbox-deploy verify release-index https://github.com/SecondStack-AI/SecondBox/releases/download/v0.1.0/secondbox-0.1.0-release-index.json
```

Keep an operator-reviewed production `secondbox.toml` input containing explicit database, object storage, application authority, secret-file, ingress, Runner placement, workspace, network, gateway, capacity, retention, and independently held guest trust-anchor choices. Materialize the deployment with:

```text
secondbox-deploy init --mode production \
  --input /protected/operator.toml \
  --release-index https://github.com/SecondStack-AI/SecondBox/releases/download/v0.1.0/secondbox-0.1.0-release-index.json \
  /srv/secondbox/deployment
secondbox-deploy compose /srv/secondbox/deployment/secondbox.toml up
```

Select `agent-compartment`, `durable-coding`, or both in `[standard_resources]`; provide only deployment inventory and gateway mappings. Do not copy their RunnerPool or Profile specifications. Reapplying the deployment validates the installed immutable lineage and appends only a missing release-owned revision.

Import the existing SDKs at the same coordinated version:

```text
go get github.com/SecondStack-AI/SecondBox@v0.1.0
npm install @secondstack-ai/secondbox@0.1.0
```

Production configuration must retain the digest-pinned control-plane, Runner, and microVM image references materialized from the verified index. Never replace them with version tags, `latest`, local builds, a source checkout, copied SDK files, copied Compose files, or consumer-owned standard-resource reconciliation.

After public finalization, the release operator records the final index, artifact manifest, and qualification URLs and digests; npm integrity; OCI digests; binary checksums; standard Profile revision/spec digests; platform matrix; and protocol windows in the GitHub release notes. Those immutable values are the canonical inputs to downstream SecondStack Agent Platform and Agent Claude integration work.
