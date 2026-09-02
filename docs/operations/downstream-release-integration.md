# Downstream release integration

Integrate v0.9.1 only after the stable GitHub Release is public. The release is the immutable `v0.9.1` tag plus the locally built files attached to that release; the publishing workflow does not rebuild them. Do not use the retracted v0.7.0 or v0.8.0 Go module coordinates.

## v0.9.1 transition

v0.9.0 was tagged but retracted before publication; pin v0.9.1 or later. From v0.9.1 the artifact manifest uses schema `secondbox.release/artifact-manifest/v6`, and downstream consumers that deploy the gVisor backend track its `gvisor` section in addition to the Firecracker `microvm` bundle:

- `ghcr.io/secondstack-ai/secondbox/runner-gvisor@sha256:...` (`gvisor.runnerReference`), the runner image.
- `ghcr.io/secondstack-ai/secondbox/gvisor-artifacts@sha256:...` (`gvisor.imageReference`), the transport carrying the flat root, launch artifacts, verifiers, and materialization.
- `secondbox-0.9.1-gvisor-materialization.json` (`gvisor.materialization`), with `gvisor.materializationDigest` and `gvisor.flatRootDigest` as the identities a node materialization must reproduce, and `gvisor.runscRelease`.
- `secondbox-0.9.1-gvisor-qualification-evidence.json` and `secondbox-0.9.1-gvisor-pod-qualification-evidence.json` (`gvisor.qualificationEvidence`, `gvisor.podQualificationEvidence`), the host and pod scenario evidence bound to the release commit.

Recorded v5 manifests of earlier releases remain readable by the v0.9.1 updater; a v0.9.1 or later release always ships v6.


Download the `secondbox-deploy_0.9.1_OS_ARCH` binary and verify it against the public `SHA256SUMS`. Then verify the published artifact manifest and every HTTP release object it references:

```text
secondbox-deploy verify artifact-manifest https://github.com/SecondStack-AI/SecondBox/releases/download/v0.9.1/secondbox-0.9.1-artifact-manifest.json
```

Keep an operator-reviewed production `secondbox.toml` input containing explicit database, platform and Runner authority, secret-file, ingress, Runner placement, workspace, network, gateway, capacity, retention, and independently held guest trust-anchor choices. It contains no application-authority file. Materialize the deployment with:

```text
secondbox-deploy init --mode production \
  --input /protected/operator.toml \
  --artifact-manifest https://github.com/SecondStack-AI/SecondBox/releases/download/v0.9.1/secondbox-0.9.1-artifact-manifest.json \
  /srv/secondbox/deployment
secondbox-deploy compose /srv/secondbox/deployment/secondbox.toml up
```

Select any explicit combination of `agent-compartment`, `durable-coding`, and `agent-compartment-isolated` in `[standard_resources]`; provide deployment inventory and only the gateway mappings required by the network-enabled selections. Do not copy their RunnerPool or Profile specifications. Reapplying the deployment validates the installed immutable lineage and appends only a missing release-owned revision.

After readiness, log in with the platform token, create each Tenant and its tenant-controller authority, log in with the returned controller token, then create the Subject and application authority. Capture each bearer token from its successful creation response; it cannot be retrieved later. The source-free CLI sequence is documented in [SDK, CLI, and Flue integration](sdk-cli-and-flue.md). The repository scenario harness uses this same sequence and creates a separate application authority for the optional `sandbox:ports:direct` grant.

Import the existing SDKs at the same coordinated version:

```text
go get github.com/SecondStack-AI/SecondBox@v0.9.1
npm install @secondstack-ai/secondbox@0.9.1
```

Production configuration must retain the digest-pinned control-plane and Runner image references and the installed verified artifact manifest. The independently configured microVM trust and asset identity must remain consistent with that manifest. Never replace release facts with version tags, `latest`, local builds, a source checkout, copied SDK files, copied Compose files, or consumer-owned standard-resource reconciliation.

After publication, record the stable release and artifact-manifest URLs, the `SHA256SUMS` and artifact-manifest digests, npm integrity, OCI digests, binary checksums, standard Profile revision/spec digests, platform matrix, and protocol windows. Those immutable values are the canonical inputs to downstream SecondStack Agent Platform and Agent Claude integration work.
