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

## Signals

- `GET /healthz` reports only that the HTTP process can answer.
- `GET /readyz` verifies PostgreSQL connectivity and returns a normal problem response on failure.
- `GET /metrics` reports counts by bounded Sandbox and Operation state values, HTTP request-duration histograms by matched route template and status class, and end-to-end Operation-duration histograms by kind and terminal state. Histogram buckets span 5 milliseconds through 120 seconds and include the standard cumulative `+Inf`, sum, and count series. It has no tenant, subject, Sandbox, Runner, request, or user labels.
- Administrative Runner projections include the retained Sandbox startup sample count and p95 reported on the authenticated runner channel. The sample count is bounded by the runner's 256-sample window; it is not a process-lifetime counter.
- Successful buffered and streaming Exec outcomes include `elapsedMilliseconds`, using the Runner-measured guest execution duration.
- Every HTTP response carries `X-Request-ID`. Clients should supply one when it is at most 128 bytes and record the returned value.
- The validated request identifier is bound to the request context and retained by asynchronous Operations, transactional audit rows, data-plane relay frames, Runner operation evidence, problem responses, and the structured HTTP completion log. The completion log records response status and elapsed milliseconds with only the method and matched route template, never the bearer credential, raw URL, query string, or workspace path.

Scrape and probe endpoints only from a trusted monitoring network. Although they contain no credentials or resource identifiers by design, readiness and state counts reveal service condition.

## Bounded support bundles

The support collector includes only the three unauthenticated probes, their HTTP status, and a bounded tail of the configured control-plane JSON log. It never collects process environments, environment files, database contents, object-store objects, workspaces, runner credentials, or guest files.

```sh
export SECONDBOX_SUPPORT_BASE_URL=http://127.0.0.1:8080
export SECONDBOX_SUPPORT_CONTROL_PLANE_LOG=/path/to/control-plane.jsonl
export SECONDBOX_SUPPORT_MAX_LOG_BYTES=1048576
export SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS=5
deploy/bin/collect-support-bundle.sh /secure/path/secondbox-support.tar.gz
```

The byte bound is mandatory and capped at 100 MiB. The command refuses to overwrite an existing archive, writes through a private temporary directory, and includes SHA-256 checksums. Application error text can still contain operationally sensitive metadata; an operator must review the archive before sharing it.

The standalone CLI provides equivalent bounded collection without requiring the deployment scripts:

```sh
secondbox \
  --url http://127.0.0.1:8080 \
  diagnostics bundle \
  --output /secure/path/secondbox-support.tar.gz \
  --control-plane-log /path/to/control-plane.jsonl \
  --max-log-bytes 1048576 \
  --http-timeout 5s
```

The CLI requires an absolute, non-symbolic-link control-plane log path, caps each probe body at 1 MiB, records transport failures or truncation in the matching status file, and creates the archive with mode `0600`. It does not send a bearer credential to the unauthenticated probes, even if the caller supplied the global `--token` option.

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
