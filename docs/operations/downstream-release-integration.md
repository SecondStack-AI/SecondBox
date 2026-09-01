# Downstream release integration

Integrate v0.8.0 only after the stable GitHub Release is public. The release is the immutable `v0.8.0` tag plus the locally built files attached to that release; the publishing workflow does not rebuild them. Do not use the retracted v0.7.0 Go module coordinate.

Download the `secondbox-deploy_0.8.0_OS_ARCH` binary and verify it against the public `SHA256SUMS`. Then verify the published artifact manifest and every HTTP release object it references:

```text
secondbox-deploy verify artifact-manifest https://github.com/SecondStack-AI/SecondBox/releases/download/v0.8.0/secondbox-0.8.0-artifact-manifest.json
```

Keep an operator-reviewed production `secondbox.toml` input containing explicit database, platform and Runner authority, secret-file, ingress, Runner placement, workspace, network, gateway, capacity, retention, and independently held guest trust-anchor choices. It contains no application-authority file. Materialize the deployment with:

```text
secondbox-deploy init --mode production \
  --input /protected/operator.toml \
  --artifact-manifest https://github.com/SecondStack-AI/SecondBox/releases/download/v0.8.0/secondbox-0.8.0-artifact-manifest.json \
  /srv/secondbox/deployment
secondbox-deploy compose /srv/secondbox/deployment/secondbox.toml up
```

Select any explicit combination of `agent-compartment`, `durable-coding`, and `agent-compartment-isolated` in `[standard_resources]`; provide deployment inventory and only the gateway mappings required by the network-enabled selections. Do not copy their RunnerPool or Profile specifications. Reapplying the deployment validates the installed immutable lineage and appends only a missing release-owned revision.

After readiness, log in with the platform token, create each Tenant and its tenant-controller authority, log in with the returned controller token, then create the Subject and application authority. Capture each bearer token from its successful creation response; it cannot be retrieved later. The source-free CLI sequence is documented in [SDK, CLI, and Flue integration](sdk-cli-and-flue.md). The repository scenario harness uses this same sequence and creates a separate application authority for the optional `sandbox:ports:direct` grant.

Import the existing SDKs at the same coordinated version:

```text
go get github.com/SecondStack-AI/SecondBox@v0.8.0
npm install @secondstack-ai/secondbox@0.8.0
```

Production configuration must retain the digest-pinned control-plane and Runner image references and the installed verified artifact manifest. The independently configured microVM trust and asset identity must remain consistent with that manifest. Never replace release facts with version tags, `latest`, local builds, a source checkout, copied SDK files, copied Compose files, or consumer-owned standard-resource reconciliation.

After publication, record the stable release and artifact-manifest URLs, the `SHA256SUMS` and artifact-manifest digests, npm integrity, OCI digests, binary checksums, standard Profile revision/spec digests, platform matrix, and protocol windows. Those immutable values are the canonical inputs to downstream SecondStack Agent Platform and Agent Claude integration work.
