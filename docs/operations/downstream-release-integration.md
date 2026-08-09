# Downstream release integration

The first release version is `0.1.0`. Do not integrate it until the stable GitHub Release is public. The release is the `v0.1.0` tag plus the locally built files attached to that release; the publishing workflow does not rebuild them.

Download the `secondbox-deploy_0.1.0_OS_ARCH` binary and verify it against the public `SHA256SUMS`. Then verify the published artifact manifest and every HTTP release object it references:

```text
secondbox-deploy verify artifact-manifest https://github.com/SecondStack-AI/SecondBox/releases/download/v0.1.0/secondbox-0.1.0-artifact-manifest.json
```

Keep an operator-reviewed production `secondbox.toml` input containing explicit database, application authority, secret-file, ingress, Runner placement, workspace, network, gateway, capacity, retention, and independently held guest trust-anchor choices. Materialize the deployment with:

```text
secondbox-deploy init --mode production \
  --input /protected/operator.toml \
  --artifact-manifest https://github.com/SecondStack-AI/SecondBox/releases/download/v0.1.0/secondbox-0.1.0-artifact-manifest.json \
  /srv/secondbox/deployment
secondbox-deploy compose /srv/secondbox/deployment/secondbox.toml up
```

Select `agent-compartment`, `durable-coding`, or both in `[standard_resources]`; provide only deployment inventory and gateway mappings. Do not copy their RunnerPool or Profile specifications. Reapplying the deployment validates the installed immutable lineage and appends only a missing release-owned revision.

Import the existing SDKs at the same coordinated version:

```text
go get github.com/SecondStack-AI/SecondBox@v0.1.0
npm install @secondstack-ai/secondbox@0.1.0
```

Production configuration must retain the digest-pinned control-plane and Runner image references and the installed verified artifact manifest. The independently configured microVM trust and asset identity must remain consistent with that manifest. Never replace release facts with version tags, `latest`, local builds, a source checkout, copied SDK files, copied Compose files, or consumer-owned standard-resource reconciliation.

After publication, record the stable release and artifact-manifest URLs, the `SHA256SUMS` and artifact-manifest digests, npm integrity, OCI digests, binary checksums, standard Profile revision/spec digests, platform matrix, and protocol windows. Those immutable values are the canonical inputs to downstream SecondStack Agent Platform and Agent Claude integration work.
