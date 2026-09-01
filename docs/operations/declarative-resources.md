# Declarative resources and standard bundles

RunnerPools and Profiles are ordinary public SecondBox resources. The control plane does not create, reserve, or reconcile standard names. A deployment or operator explicitly applies a versioned `secondbox.resources/v1` document through the shared Go resource engine.

Use the CLI to preview or converge a reviewed document:

```sh
secondbox resources check --file resources.json
secondbox resources apply --file resources.json
```

Both commands return structured per-resource actions. `check` performs no mutations. `apply` creates RunnerPools before dependent Profiles, uses optimistic RunnerPool and Profile revisions, and uses deterministic idempotency keys for immutable Profile appends. Exact resources are no-ops. An absent resource is never deleted or drained; pruning is not part of this release.

A Profile declaration contains its complete ordered lineage. Every revision carries its canonical SHA-256 spec digest. On install, the engine verifies every installed historical revision against the declared prefix and then appends missing revisions sequentially. Gaps, changed historical specs, disabled heads, unknown future heads, incompatible architecture/capability drift, and update races fail explicitly.

The release owns three amd64 standard bundles:

- `agent-compartment` is short-lived, has no public Ports or Snapshots, and can reach only `agent-gateway.secondbox.internal` over HTTPS.
- `durable-coding` is a bounded long-lived workspace with Snapshots, terminal detach, and the named `development-http` Port; it can reach only `platform-gateway.secondbox.internal` over HTTPS.
- `agent-compartment-isolated` has the command, file, workspace, cancellation, and bounded lifecycle surface of `agent-compartment`, but its network policy is `deny_all`, it exposes no Ports, and it requires no logical gateway.

The bundle resolver takes runtime/toolchain identity from the verified release artifact manifest. Standard documents contain no tokens, application authorities, runner credentials, host paths, or storage keys.

## Deployment selection

`secondbox.toml` requires an explicit `[standard_resources]` section with the verified artifact-manifest path, selected bundle names, apply readiness bound, and one typed RunnerPool inventory binding per selected bundle. Production uses the same shape and accepts no generated development authority.

Logical gateway addresses remain Runner-local deployment configuration. Every declared Runner in a selected pool that should admit a network-enabled standard Profile must advertise one or more contexts and map that Profile's logical name inside each applicable context, for example:

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

A logical gateway mapping binds the Profile's exact destination name and port to one protected Runner-local address for network-policy authorization. It does not provide guest name resolution or authenticate the destination. The Runner DNS proxy forwards guest questions to its configured upstream only; it does not synthesize an answer for the logical gateway name, and it rejects upstream answers that resolve to protected addresses. SecondBox does not assume that the destination is an HTTP proxy, inject proxy variables into Exec requests, or distribute an external proxy's interception CA. An application that supplies a forward proxy must make its logical name resolvable in the deployment's upstream DNS, pass its proxy environment explicitly, and make the proxy CA available in the guest trust path. The application also owns hostname verification and CA rotation. Gateway reachability is qualified in the production deployment, not supplied by the standard bundle. This keeps external gateway authority out of immutable SecondBox runtime and toolchain bundles.

`agent-compartment-isolated` does not add a gateway mapping and explicitly declines a Tenant context. `agent-compartment` and `durable-coding` explicitly require one. The release documents and artifact manifest bind the new immutable standard Profile heads and exact revision numbers; operators consume those documents rather than reconstructing revision identities. The Runner configuration contains only context names, logical names, and Runner-local IP addresses; gateway certificates, interception authority, proxy endpoints, policy databases, and credentials stay outside SecondBox.

`secondbox-deploy inspect` shows selected bundles, each standard Profile's release identity, and each Runner's advertised context names without exposing mapping addresses or host paths. `secondbox-deploy compose ... up` waits for `/readyz` and applies the selected document using the same library as `secondbox resources apply`.
