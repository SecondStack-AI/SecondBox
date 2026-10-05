# Declarative resources and standard bundles

RunnerPools and Profiles are ordinary public SecondBox resources. The control plane does not create, reserve, or reconcile standard names. A deployment or operator explicitly applies a versioned `secondbox.resources/v1` document through the shared Go resource engine.

Use the CLI to preview or converge a reviewed document:

```sh
secondbox resources check --file resources.json
secondbox resources apply --file resources.json
```

Both commands return structured per-resource actions. `check` performs no mutations. `apply` creates RunnerPools before dependent Profiles, uses optimistic RunnerPool and Profile revisions, and uses deterministic idempotency keys for immutable Profile appends. Exact resources are no-ops. An absent resource is never deleted or drained; pruning is not part of this release.

A Profile declaration contains its complete ordered lineage. Every revision carries its canonical SHA-256 spec digest. On install, the engine verifies every installed historical revision against the declared prefix and then appends missing revisions sequentially. Gaps, changed historical specs, disabled heads, unknown future heads, incompatible architecture/capability drift, and update races fail explicitly.

Documents written before v0.19.0 carry `runtimeBundleDigest` and `toolchainBundleDigest` in every revision spec. Delete both fields from each revision, keep the revision numbers, and replace each `specDigest` with the value `secondbox resources check` reports as `spec is sha256:…` for that revision. Migration `0031_profile_execution_assets_unpinned` removes the same fields from installed revisions, so the updated document converges on the installed history without new revisions.

The release owns three standard bundles for its guest architecture, `amd64` or `arm64`:

- `agent-compartment` defaults to 60-second idle shutdown and unlimited maximum runtime, has no public Ports or Snapshots, and can reach only `agent-gateway.secondbox.internal` over HTTPS.
- `durable-coding` is a bounded long-lived workspace with Snapshots, terminal detach, and the named `development-http` Port; it can reach only `platform-gateway.secondbox.internal` over HTTPS.
- `agent-compartment-isolated` has the same lifecycle defaults and command, file, workspace, and cancellation surface as `agent-compartment`, but its network policy is `deny_all`, it exposes no Ports, and it requires no logical gateway.

Both Agent bundles declare unlimited idle/runtime delegation ceilings. Subject selections
apply at new Sandbox creation; existing Sandboxes keep their pinned lifecycle. See
[configurable limits](../design/configurable-limits.md) for `lifecycleCeiling` omission and
explicit-null semantics. Individual commands and ownership leases remain bounded.

Standard Profiles name no execution bundle; each Instance boots the signed bundle its home Runner has installed. Standard documents contain no tokens, application authorities, runner credentials, host paths, or storage keys.

## Deployment selection

`secondbox.toml` requires an explicit `[standard_resources]` section with selected bundle names, apply readiness bound, typed RunnerPool inventory declared once by name, and exactly one source of the release guest architecture:

- `artifact_manifest`: the path of the verified release artifact manifest. The manifest names the guest architecture, the standard lineages must equal the Profile identities it released, and in production every standard-pool Runner's `artifact_public_key_sha256` must equal its microVM signing fingerprint.
- `guest_architecture`: `amd64` or `arm64`, for a deployment built from source that has no release artifact manifest. `secondbox-deploy` builds the standard documents from its own code for that architecture. Its build identity selects the lineage: the unstamped development identity selects the short development lineage, and any stamped release identity selects the full published lineage. A binary that stamps only one of version and source commit is rejected. Each Runner keeps its own explicit `artifact_public_key_sha256`.

Setting both or neither fails validation. All three standard bundles use the standard pool of the release guest architecture, `standard-amd64` or `standard-arm64`, so any combination of them shares one `[[standard_resources.runner_pools]]` declaration whose `architectures` include that architecture. `init --mode development` declares the pool of the host that runs it. Duplicate pool names are rejected. Production uses the same shape and accepts no generated development authority.

Logical gateway addresses remain Runner-local deployment configuration. Every declared Runner in a selected pool that should admit a network-enabled standard Profile must advertise one or more contexts and map that Profile's logical name inside each applicable context. This example declares a remote configuration path; omit `egress_context_config_path` for same-host placement:

```toml
egress_context_config_path = "/etc/secondbox/egress-contexts.json"

[[runners.egress_contexts]]
name = "secondstack-staging"

[[runners.egress_contexts.gateways]]
logical_name = "agent-gateway.secondbox.internal"
address = "10.0.0.10"

[[runners.egress_contexts.gateways]]
logical_name = "platform-gateway.secondbox.internal"
address = "10.0.0.11"
```

A logical gateway mapping binds the Profile's exact destination name and port to one protected Runner-local address for network-policy authorization. It does not provide guest name resolution or authenticate the destination. The Runner DNS proxy forwards guest questions to its configured upstream only; it does not synthesize an answer for the logical gateway name, and it rejects upstream answers that resolve to protected addresses. SecondBox does not assume that the destination is an HTTP proxy, inject proxy variables into Exec requests, or distribute an external proxy's interception CA. An application that supplies a forward proxy must pass the admitted Runner-local gateway IP explicitly in its proxy environment and make the proxy CA available in the guest trust path. Publishing a private-IP DNS answer for the logical name does not bypass the DNS protection rules. The application also owns hostname verification and CA rotation. Gateway reachability is qualified in the production deployment, not supplied by the standard bundle. This keeps external gateway authority out of immutable SecondBox runtime and toolchain bundles.

`agent-compartment-isolated` does not add a gateway mapping and explicitly declines a Tenant context. `agent-compartment` and `durable-coding` explicitly require one. The release documents and artifact manifest bind the new immutable standard Profile heads and exact revision numbers; operators consume those documents rather than reconstructing revision identities. The Runner context configuration contains context names, logical names, Runner-local IP addresses, and optional attributed Unix socket paths; gateway certificates, interception authority, proxy endpoints, policy databases, and credentials stay outside SecondBox.

`secondbox-deploy inspect` shows selected bundles, each standard Profile's release identity, and each Runner's advertised context names without exposing mapping addresses or host paths. `secondbox-deploy compose ... up` waits for `/readyz` and applies the selected document using the same library as `secondbox resources apply`.

RunnerPools declare placement inventory: name, state, architectures, and capabilities. Remove `capacityPolicy` from resource documents and API requests, remove it from `mutableFields`, and remove `--capacity` from standard-bundle CLI commands. In deployment manifests, remove `max_sandboxes`, `max_vcpu_count`, and `max_memory_bytes` from `[[standard_resources.runner_pools]]`. These pool settings were never enforced. Tenant and Subject quotas, Profile resource ceilings, and reported Runner capacity continue to govern admission. The forward migration drops only the unused pool metadata column; deploy the control plane and clients together because old clients and old control-plane binaries still expect that field.
