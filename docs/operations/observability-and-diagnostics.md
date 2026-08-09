# Observability and diagnostics

SecondBox exposes unauthenticated liveness, readiness, and fixed-cardinality Prometheus metrics. Operational logs are newline-delimited JSON on stdout and at the explicit absolute `SECONDBOX_LOG_PATH`. Security-sensitive domain mutations write transactional rows to `secondbox.audit_events`.

The standalone CLI can read or follow that local file with an explicit initial byte bound:

```sh
secondbox logs tail \
  --path /var/log/secondbox/control-plane.jsonl \
  --bytes 1048576

secondbox logs follow \
  --path /var/log/secondbox/control-plane.jsonl \
  --bytes 1048576 \
  --poll-interval 250ms
```

These local commands do not require or transmit API credentials. The path must be an absolute regular file; symbolic links are rejected initially and after replacement. Following survives ordinary regular-file truncation and replacement and ends when the CLI process is interrupted.

The credentialed commands below show fully explicit flags. Each of `--url`, `--token`, `--tenant-ref`, and `--subject-ref` may instead come from the environment or from stored configuration; see [SDK, CLI, and Flue quick starts](sdk-cli-and-flue.md).

## Signals

- `GET /healthz` reports only that the HTTP process can answer.
- `GET /readyz` verifies PostgreSQL connectivity and returns a normal problem response on failure.
- `GET /metrics` reports counts by bounded Sandbox and Operation state values; HTTP request-duration histograms by matched route template and status class; end-to-end Operation-duration histograms by kind and terminal state; Sandbox startup and startup-stage histograms; and Exec histograms by buffered or streaming mode and terminal outcome. Histogram buckets span 5 milliseconds through 120 seconds and include the standard cumulative `+Inf`, sum, and count series. It has no tenant, subject, Sandbox, Runner, request, or user labels.
- Administrative Runner projections include the retained Sandbox startup sample count and p95 reported on the authenticated runner channel. The sample count is bounded by the runner's 256-sample window; it is not a process-lifetime counter.
- Successful buffered and streaming Exec outcomes include `elapsedMilliseconds`, using the Runner-measured guest execution duration.
- Every HTTP response carries `X-Request-ID`. Clients should supply one when it is at most 128 bytes and record the returned value.
- The validated request identifier is bound to the request context and retained by asynchronous Operations, transactional audit rows, data-plane sessions, Runner operation evidence, problem responses, and the structured HTTP completion log. The completion log records response status and elapsed milliseconds with only the method and matched route template, never the bearer credential, raw URL, query string, or workspace path.

Scrape and probe endpoints only from a trusted monitoring network. Although they contain no credentials or resource identifiers by design, readiness and state counts reveal service condition.

## Timing inspection

The authenticated timing projection uses the same persisted Operation, Assignment progress, and Exec-session evidence that backs the database-derived metrics. The API portion uses the same fixed-cardinality in-process recorder as `/metrics`.

Read a bounded Sandbox history:

```sh
secondbox \
  --url https://secondbox.example \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref tenant_123 \
  --subject-ref subject_123 \
  timings sandbox \
  --sandbox-id sbox_123 \
  --limit 50
```

`--limit` is required and must be from 1 through 200. It independently bounds the recent Operation list and recent successful or deadline-exceeded Exec list. Each Operation row separates queue, execution, and total time. Startup Operations also include one provider-neutral stage table per Sandbox generation.

Read one Operation:

```sh
secondbox \
  --url https://secondbox.example \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref tenant_123 \
  --subject-ref subject_123 \
  timings operation \
  --operation-id op_123
```

Read the current deployment:

```sh
secondbox \
  --url https://secondbox.example \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref secondbox \
  --subject-ref secondbox-admin \
  timings summary \
  --window 15m
```

The window is required, uses whole seconds, and must be from one minute through one hour. The output reports p50, p95, and p99 for boot, Exec, and API latency; per-stage boot percentiles and the stage with the highest p95; per-mode Exec series; matched API route and status-class series; and Operation queue, execution, and total p95. API windows use fixed one-minute buckets, so a requested interval can include the remainder of its first minute. API samples are process-local and reset when the control-plane process restarts. Database-derived boot, Exec, and Operation samples survive a restart for as long as their owning records are retained.

Startup `observedAt` is the runner timestamp retained from the control channel; `receivedAt` is the control-plane receipt timestamp. Stage elapsed values use consecutive runner timestamps. The first stage uses Assignment creation as its baseline and therefore includes dispatch time and depends on runner/control-plane clock synchronization. Compare `observedAt` and `receivedAt` when diagnosing clock skew.

The underlying authenticated routes are `GET /v1/sandboxes/{sandboxId}/timings?limit=...`, `GET /v1/operations/{operationId}/timings`, and `GET /v1/timings?windowSeconds=...`. They expose no host paths, Runner identity, backend vocabulary, storage references, or fencing material.

For a real-compute concurrency sweep that records these timing projections alongside workload
throughput and saturation evidence, see
[Local stress qualification](stress-qualification.md). The stress gate requires KVM and the signed
artifact bundle and never substitutes mock compute.

## Bounded support bundles

The support collector includes the three unauthenticated probes, their HTTP status, an authenticated aggregate timing summary, and a bounded tail of the configured control-plane JSON log. It never collects process environments, environment files, credentials, database contents, workspaces, runner credentials, or guest files. The platform token is sent only to the aggregate timing route and is never written to the archive.

```sh
export SECONDBOX_SUPPORT_BASE_URL=http://127.0.0.1:8080
export SECONDBOX_SUPPORT_CONTROL_PLANE_LOG=/path/to/control-plane.jsonl
export SECONDBOX_SUPPORT_MAX_LOG_BYTES=1048576
export SECONDBOX_SUPPORT_MAX_PROBE_BYTES=1048576
export SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS=5
export SECONDBOX_SUPPORT_TIMING_WINDOW_SECONDS=900
export SECONDBOX_SUPPORT_PLATFORM_TOKEN="$SECONDBOX_PLATFORM_TOKEN"
deploy/bin/collect-support-bundle.sh /secure/path/secondbox-support.tar.gz
```

Both byte bounds are mandatory. The log tail is capped at 100 MiB and each HTTP response at 10 MiB. The timing window must be from 60 through 3600 seconds. The command refuses to overwrite an existing archive, writes through a private temporary directory, and includes SHA-256 checksums. Application error text can still contain operationally sensitive metadata; an operator must review the archive before sharing it.

The standalone CLI provides equivalent bounded collection without requiring the deployment scripts:

```sh
secondbox \
  --url http://127.0.0.1:8080 \
  diagnostics bundle \
  --output /secure/path/secondbox-support.tar.gz \
  --control-plane-log /path/to/control-plane.jsonl \
  --max-log-bytes 1048576 \
  --max-probe-bytes 1048576 \
  --http-timeout 5s \
  --timing-window 15m
```

The CLI requires absolute output and non-symbolic-link control-plane log paths, explicit bounds, and an explicit timing window. It records transport failures or truncation in the matching status file and creates the archive with mode `0600`. It does not send the bearer credential to the unauthenticated probes; it sends it only to the aggregate timing route.

## Audit inspection

There is no public audit-list HTTP route in this release. A database operator with a read-only PostgreSQL role can export up to 10,000 recent events:

```sh
export SECONDBOX_AUDIT_DATABASE_URL='postgresql://audit_reader@database/secondbox?sslmode=verify-full'
export SECONDBOX_AUDIT_LIMIT=1000
export SECONDBOX_AUDIT_CONNECT_TIMEOUT_SECONDS=5
deploy/bin/export-audit.sh /secure/path/secondbox-audit.jsonl
```

The export includes project, actor, resource, action, outcome, request, details, and timestamp fields. It refuses to overwrite and creates mode `0600` output. Treat it as confidential tenant metadata. The database role should have only `SELECT` on `secondbox.audit_events`.

## Runner host diagnostic

Run the host collector locally on a Runner host:

```sh
export SECONDBOX_RUNNER_SYSTEMD_UNIT=secondbox-runner.service
export SECONDBOX_RUNNER_DIAGNOSTIC_JOURNAL_LINES=2000
deploy/bin/diagnose-runner-host.sh /secure/path/secondbox-runner-diagnostic.txt
```

It records bounded journal output, systemd state, `/dev/kvm`, cgroup filesystem type, kernel information, and filesystem capacity. It does not read the Runner environment file or mTLS keys. Review journal content before sharing because guest-controlled error text is untrusted.

The runner binary also implements `secondbox-runner --healthcheck`, which opens the authenticated runner protocol stream and exits. It requires the complete runner environment, the pre-shared Runner credential, and a CA-signed client certificate. Runner gRPC has no unauthenticated health endpoint; control-plane HTTP readiness currently proves PostgreSQL connectivity but not acceptance of a Runner stream.
