# Downstream release integration

Choose a published stable release, read its deployment boundary, and use that
version consistently for binaries, SDKs, images, and standard bundles. In the
examples below, replace `VERSION` with the chosen numeric version. The tag and
attached files identify one release; the publishing workflow does not rebuild them.

For [v0.18.0](../releases/v0.18.0.md), a v0.17.0 deployment updates in place: the Firecracker bundle, trust anchor, Runner protocol generation 5, and migration baseline are unchanged. gVisor consumers must deploy the image fetcher, a reflink-capable execution image cache, registry configuration, and the publisher key beside every gVisor Runner before updating; see the release notes for the settings.

For [v0.17.0](../releases/v0.17.0.md), the new signed Firecracker bundle changes runtime and toolchain identities. The guided updater refuses an in-place update. Retire Sandboxes, retain a coordinated backup, reinstall with a fresh database and separate Runner storage root, and recreate resources against the new bundle and standard revisions. The RSA trust anchor from v0.12.0 is retained. The release keeps Runner protocol generation 5 and the v0.15.0 migration baseline.

Historically, [v0.16.0](../releases/v0.16.0.md) allowed v0.15.0 to update in place, and [v0.15.0](../releases/v0.15.0.md) accepted the exact v0.14.0 migration baseline. Other clean-install boundaries still require the procedure in their target release notes.
The Go module's `retract` directives identify withdrawn versions.

## gVisor and the v6 artifact manifest

The current artifact manifest uses schema `secondbox.release/artifact-manifest/v6`, and downstream consumers that deploy the gVisor backend track its `gvisor` section in addition to the Firecracker `microvm` bundle:

- `ghcr.io/secondstack-ai/secondbox/runner-gvisor@sha256:...` (`gvisor.runnerReference`), the runner image. Since v0.18.0 it also runs the unprivileged `secondbox-image-fetcher` container that every gVisor Runner requires.
- `ghcr.io/secondstack-ai/secondbox/gvisor-artifacts@sha256:...` (`gvisor.imageReference`), the transport carrying the flat root, launch artifacts, verifiers, and materialization.
- `secondbox-VERSION-gvisor-materialization.json` (`gvisor.materialization`), with `gvisor.materializationDigest` and `gvisor.flatRootDigest` as the identities a node materialization must reproduce, and `gvisor.runscRelease`.
- `secondbox-VERSION-gvisor-qualification-evidence.json` and `secondbox-VERSION-gvisor-pod-qualification-evidence.json` (`gvisor.qualificationEvidence`, `gvisor.podQualificationEvidence`), the host evidence and, for a full release, pod evidence bound to the release commit. Default releases do not carry pod qualification; consumers requiring it must select a full release.

Recorded v5 manifests remain readable for legacy release verification. Schema readability alone does not imply update compatibility; check database, bundle, and protocol boundaries separately.

Download the `secondbox-deploy_VERSION_OS_ARCH` binary and verify it against the public `SHA256SUMS`. Then verify the published artifact manifest and every HTTP release object it references:

```text
secondbox-deploy verify artifact-manifest https://github.com/SecondStack-AI/SecondBox/releases/download/vVERSION/secondbox-VERSION-artifact-manifest.json
```

Keep an operator-reviewed production `secondbox.toml` input containing explicit database, platform and Runner authority, secret-file, ingress, Runner placement, workspace, network, gateway, capacity, retention, and independently held guest trust-anchor choices. It contains no application-authority file. Materialize the deployment with:

```text
secondbox-deploy init --mode production \
  --input /protected/operator.toml \
  --artifact-manifest https://github.com/SecondStack-AI/SecondBox/releases/download/vVERSION/secondbox-VERSION-artifact-manifest.json \
  /srv/secondbox/deployment
secondbox-deploy compose /srv/secondbox/deployment/secondbox.toml up
```

Select any explicit combination of `agent-compartment`, `durable-coding`, and `agent-compartment-isolated` in `[standard_resources]`; provide deployment inventory and only the gateway mappings required by the network-enabled selections. Do not copy their RunnerPool or Profile specifications. Reapplying the deployment validates the installed immutable lineage and appends only a missing release-owned revision.

For attributed execution, select an `agent-compartment` revision that declares
`attributedExecution`, and configure its gateway's `attributed_socket` on the
Runner host. Existing Sandboxes retain their pinned revision; an attributed generation resolves the numeric connection grant from the current Profile head on its next Assignment. Read the actual
revision and spec digest from the selected release's standard bundle rather
than deriving revision numbers from a version. The isolated Profile has no
attributed permission. Read the public API and Runner protocol windows from the selected manifest
(v0.18.0 uses version 1 and `[5,5]`). Attributed execution also requires the advertised `attributed-execution`
capability. See [Profiles and authorization](../design/profiles-and-authorization.md).

After readiness, log in with the platform token, create each Tenant and its tenant-controller authority, log in with the returned controller token, then create the Subject and application authority. Capture each bearer token from its successful creation response; it cannot be retrieved later. The source-free CLI sequence is documented in [SDK, CLI, and Flue integration](sdk-cli-and-flue.md). The repository scenario harness uses this same sequence and creates a separate application authority for the optional `sandbox:ports:direct` grant.

Import the existing SDKs at the same coordinated version:

```text
go get github.com/SecondStack-AI/SecondBox@vVERSION
npm install @secondstack-ai/secondbox@VERSION
```

Production configuration must retain the digest-pinned control-plane and Runner image references and the installed verified artifact manifest. The independently configured microVM trust and asset identity must remain consistent with that manifest. Never replace release facts with version tags, `latest`, local builds, a source checkout, copied SDK files, copied Compose files, or consumer-owned standard-resource reconciliation.

After publication, record the stable release and artifact-manifest URLs, the `SHA256SUMS` and artifact-manifest digests, npm integrity, OCI digests, binary checksums, standard Profile revision/spec digests, platform matrix, and protocol windows. Those immutable values are the canonical inputs to downstream SecondStack Agent Platform and Agent Claude integration work.

### Attributed connection policy adoption

Deploy a control plane and client with the delegated Subject connection contract,
then explicitly apply the updated standard `agent-compartment` bundle. Its appended
revision grants default 128 and ceiling 4096. Code deployment alone cannot raise an
operator-owned grant. Existing attributed Sandboxes, including old two-connection
pins, adopt the new numeric default on their next Assignment without recreation;
their pinned gateway and all other authority remain unchanged. Active Assignments
retain their admitted limit. The existing runner protocol already transports the
finite numeric bound; retain normal compatible control-plane/runner release pins.

CT writes the complete Subject Sandbox policy, preserving lifecycle and connection
selections when editing either. Its first compatible upstream release must include
the unchanged-block allowance for both lifecycle and attributed selections: a saved
value for the same Profile remains valid in a complete PUT after operator tightening
or removal of attributed permission. Effective resolution still enforces the grant.
CT must not discard the saved desired value by clamping it before PUT.
It can display prospective default, effective value,
and ceiling from the policy read response, but must not label that value as active
generation state. Downstream proxy limits are independent. No release version,
artifact digest, or capacity claim is implied by this contract; use the actual
published and verified release when updating downstream pins.
