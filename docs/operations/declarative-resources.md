# Declarative resources and standard bundles

RunnerPools and Profiles are ordinary public SecondBox resources. The control plane does not create, reserve, or reconcile standard names. A deployment or operator explicitly applies a versioned `secondbox.resources/v1` document through the shared Go resource engine.

Use the CLI to preview or converge a reviewed document:

```sh
secondbox resources check --file resources.json
secondbox resources apply --file resources.json
```

Both commands return structured per-resource actions. `check` performs no mutations. `apply` creates RunnerPools before dependent Profiles, uses optimistic RunnerPool and Profile revisions, and uses deterministic idempotency keys for immutable Profile appends. Exact resources are no-ops. An absent resource is never deleted or drained; pruning is not part of this release.

A Profile declaration contains its complete ordered lineage. Every revision carries its canonical SHA-256 spec digest. On install, the engine verifies every installed historical revision against the declared prefix and then appends missing revisions sequentially. Gaps, changed historical specs, disabled heads, unknown future heads, incompatible architecture/capability drift, and update races fail explicitly.

The release owns two amd64 standard bundles:

- `agent-compartment` is short-lived, has no public Ports or Snapshots, and can reach only `agent-gateway.secondbox.internal` over HTTPS.
- `durable-coding` is a bounded long-lived workspace with Snapshots, Artifacts, terminal detach, and the named `development-http` Port; it can reach only `platform-gateway.secondbox.internal` over HTTPS.

The bundle resolver takes runtime/toolchain identity from the verified release artifact manifest. Standard documents contain no tokens, application authorities, runner credentials, host paths, or storage keys.

## Deployment selection

`secondbox.toml` requires an explicit `[standard_resources]` section with the verified artifact-manifest path, selected bundle names, apply readiness bound, and one typed RunnerPool inventory binding per selected bundle. Production uses the same shape and accepts no generated development authority.

Logical gateway addresses remain Runner-local deployment configuration. Every declared Runner in a selected pool must map the required logical name in `network_policy_runner_gateways`, for example:

```toml
network_policy_runner_gateways = "agent-gateway.secondbox.internal=10.0.0.10,platform-gateway.secondbox.internal=10.0.0.11"
```

`secondbox-deploy inspect` shows selected bundles plus each standard Profile's release identity: name, revision number, and spec digest. `secondbox-deploy compose ... up` waits for `/readyz` and applies the selected document using the same library as `secondbox resources apply`.
