# CLI output and terminal presentation

`secondbox` and `secondbox-deploy` share the terminal policy in
`internal/cliui`. Presentation is selected independently for standard output
and standard error. Redirecting stdout does not disable status on a TTY stderr,
and a TTY stdout never permits presentation bytes in a raw payload.

`secondbox` accepts these global options before its command:

```text
--output auto|json|plain
--color auto|always|never
--accessible
```

`auto` preserves the historical bytes unless stdout is an eligible TTY and the
command has a bounded human view. `json` selects the original machine JSON;
validated response bytes are copied rather than decoded and re-encoded.
`plain` selects an unstyled, deterministic human view. `NO_COLOR` disables
automatic color, while an explicit `--color always` wins over `NO_COLOR`.
`TERM=dumb` and CI disable automatic styling. `--accessible` and
`SECONDBOX_ACCESSIBLE=1` select Huh's line-oriented accessible prompts.

The equivalent flags may precede a `secondbox-deploy` command. A subcommand's
own `--output FILE` option follows the command and retains its existing meaning.

## Permanent output classifications

The tables below mirror the executable coverage registries. Adding a command
without a classification fails command tests.

| `secondbox` surface | stdin | stdout authority | stderr and exit owner |
|---|---:|---|---|
| `version`, typed `platform/controller/application login`, `logout`, `whoami` | no | bounded human or explicit JSON/plain | CLI |
| bounded Tenant, controller-authority, Subject, application-authority, and usage reads | no | human only when explicitly selected or eligible; otherwise original API JSON | API errors; CLI exit |
| bounded `profiles`, `runner-pools`, `runners`, `sandboxes`, `snapshots`, `leases`, and `ports` list/get aliases | request body only where declared | human only when explicitly selected or eligible; otherwise original API JSON | API errors; CLI exit |
| management mutations, mutating Sandbox aliases, and `resources check/apply` | optional request/file input | machine JSON; credential creation and rotation include the one-time bearer token | API/CLI |
| `operation OPERATION_ID` | operation-defined | original response bytes | API |
| `files read`, logs | no | raw file or log bytes | CLI/API |
| `run`, `exec`, `shell`, `sandbox shell`, `exec stream` | guest stdin/control stream | guest stdout/control bytes | guest stderr and guest exit status |
| timings and diagnostics receipt | no | bounded report or declared archive/path | CLI |

| `secondbox-deploy` surface | stdin | stdout authority | stderr and exit owner |
|---|---:|---|---|
| `version`, `init`, `validate` | no | bounded receipt with historical non-TTY form | CLI |
| `runner-template`, `render`, `runner-init` | no | exact TOML, environment, path, or generated artifact contract | CLI |
| `verify`, `inspect` | no | machine JSON | CLI |
| `compose` | Docker-defined | Docker Compose stdout, connected directly | Docker Compose stderr and exit status |

Raw commands ignore human styling. Guest stdin, stdout, stderr, terminal resize,
detach, and reconnect frames are never passed through `internal/cliui`. Docker
Compose output is not parsed as deployment state.

Repository automation relies on these exact machine paths:

```text
go run ./cmd/secondbox-deploy inspect "$manifest" | jq ...
go run ./cmd/secondbox-deploy runner-template > runner.toml
secondbox sandboxes list | jq ...
secondbox files read ... > workspace-file
```

Use `--output json` in new scripts when a bounded command also has a human
view. Use the generic `operation` command when byte-for-byte access to a public
operation response is required.

Stored sessions include an explicit authority kind. A platform session cannot
be populated with a controller credential, a controller session cannot be used
for Sandbox commands, and only an application session stores Tenant and Subject
references. Authority reads and lists never print bearer material. Creation and
rotation output is the only recovery point for a generated bearer token; route
redirected JSON immediately into protected secret handling.

## Release footprint measurement

Measured from the pre-change `HEAD` and this implementation with Go 1.25.12 on
Linux amd64 (100 warm process starts, `version` redirected to `/dev/null`):

| Binary | Before | With CLI UI | 100 starts before / after |
|---|---:|---:|---:|
| `secondbox` | 10,666,166 bytes | 13,634,533 bytes | 0.157 s / 0.174 s |
| `secondbox-deploy` | 20,415,725 bytes | 21,789,391 bytes | 0.197 s / 0.214 s |

The repository declares no release binary-size or cold-start budget. The
temporary baseline checkout and binaries used for this measurement were removed
afterward.
