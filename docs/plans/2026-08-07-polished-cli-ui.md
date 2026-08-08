# Plan: Polished Human-Facing CLI UI

Give `secondbox` and `secondbox-deploy` one deliberate, accessible terminal presentation system without changing their command parsers or weakening their Unix contracts. Interactive terminals receive compact styled help, tables, summaries, forms, progress, and actionable errors; pipes, redirected output, explicit machine modes, guest streams, and subprocess exit statuses remain deterministic and free of presentation bytes.

This plan is the UI prerequisite for [Guided Single-Host Installation](2026-08-07-guided-single-host-install.md). It covers only the two human-facing binaries. It does not move `secondboxd`, `secondbox-runner`, guest agents, release tools, generators, probes, or test drivers onto a TUI framework; add a full-screen application; migrate argument parsing to Cobra; style workspace/file payloads; or change public API schemas.

Use the mutually compatible stable Charm v2 modules pinned in `go.mod`: Huh for forms, Lip Gloss for deterministic inline rendering, and Bubble Tea/Bubbles only for inline progress that needs an event loop. Retain the standard `flag` and existing explicit dispatch code for arguments.

## Validation Commands

- `go test ./internal/cliui ./cmd/secondbox ./cmd/secondbox-deploy -count=1`
- `just verify-generated`
- `just lint`
- `just test`
- `just test-contract`
- `just test-deployment`
- `just test-compose`
- `just test-sdk-packages`
- `just test-cli-ui`
- `just test-non-kvm`
- `just test-scenario`

### Task 1: Freeze existing CLI output and stream contracts

Establish byte-level baselines before adding presentation logic. The tests from this task define which surfaces may gain an automatic human renderer and which remain permanent raw-data paths.

- [ ] Inventory every `secondbox` and `secondbox-deploy` command by stdin use, stdout type, stderr behavior, TTY requirements, JSON/TOML/file payload, long-running status, subprocess delegation, and exit-status ownership.
- [ ] Classify `run`, `exec`, `shell`, `sandbox shell`, `exec stream`, file reads, artifact downloads, log streaming, generic `operation`, `runner-template`, `render`, `inspect`, and `verify` as raw or machine-authoritative surfaces unless an explicit human renderer is selected.
- [ ] Add byte-for-byte fixtures for current non-TTY stdout, stderr, and exit status across successful commands, parse errors, API errors, login/logout/whoami, deployment validation, Compose delegation, and version output.
- [ ] Add regression tests proving guest stdout and stderr stay separate, guest exit status wins, stdin reaches the guest unchanged, terminal resize/control bytes are untouched, and a disconnected stream performs the existing cleanup.
- [ ] Record commands currently used through command substitution, `jq`, redirection, or pipelines in repository scripts and make those exact invocations contract fixtures.
- [ ] Define one `OutputMode` contract: `auto` selects styled human output only on an eligible TTY, `json` preserves machine JSON, and `plain` emits unstyled human output; raw byte-stream commands ignore human styling altogether.
- [ ] Define one `ColorMode` contract: `auto`, `always`, and `never`, with `NO_COLOR` overriding automatic color but not an explicit `always` request.
- [ ] Add a generated or table-driven coverage test that fails when a new command lacks an output classification.

### Task 2: Create the shared terminal capability and theme foundation

Add `internal/cliui` as the sole package allowed to know about Charm rendering. Command and domain packages provide typed data and errors; they never construct styles or inspect terminal globals themselves.

- [ ] Add pinned stable dependencies compatible with Go 1.25: `charm.land/huh/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2`, and `charm.land/bubbletea/v2`; verify module provenance and licenses and update third-party notices where required.
- [ ] Define an injectable `Capabilities` probe with independent stdin, stdout, and stderr TTY state, terminal dimensions, color profile, light/dark background, Unicode support, `TERM=dumb`, `NO_COLOR`, CI, and accessible mode.
- [ ] Never probe terminal background or capabilities through the wrong stream: data rendering follows stdout, while progress and diagnostic rendering follow stderr.
- [ ] Define a compact SecondBox theme with one adaptive accent, neutral primary/muted text, semantic success/warning/error colors, and 1-bit/ASCII fallbacks.
- [ ] Encode visual hierarchy through weight, alignment, and spacing rather than decorative gradients, oversized banners, animation for its own sake, or boxes around every section.
- [ ] Expose output, diagnostic, and interactive handles explicitly; do not read or replace `os.Stdin`, `os.Stdout`, or `os.Stderr` inside reusable renderers.
- [ ] Add global `--output auto|json|plain`, `--color auto|always|never`, and `--accessible` parsing to `secondbox` without changing the precedence or spelling of existing global authority flags.
- [ ] Add corresponding presentation flags only where `secondbox-deploy` produces human output; raw template/JSON/environment outputs must retain their existing bytes by default.
- [ ] Add capability and theme tests for light/dark, 1/16/256/true-color, Unicode/ASCII, TTY/non-TTY, redirected stdout with TTY stderr, CI, `TERM=dumb`, `NO_COLOR`, explicit overrides, and fixed terminal sizes.

### Task 3: Implement reusable plain, styled, and JSON presentation components

Build a deliberately small component library so both binaries share one language without turning `internal/cliui` into a second application framework. Every human view must render from a typed presentation model through either the styled or plain renderer.

- [ ] Implement command headers, section headings, key/value summaries, status badges, warnings, errors, next-command hints, and final receipts with styled and ASCII/plain equivalents.
- [ ] Implement width-aware tables with declared column priority, truncation policy, stable row order, and a stacked narrow-terminal fallback rather than uncontrolled wrapping.
- [ ] Implement phase lists with pending, active, complete, warning, and failed states; completed phases collapse to one stable line.
- [ ] Implement inline determinate progress and spinners on stderr using Bubble Tea/Bubbles only when stderr is an eligible TTY; otherwise emit bounded start/completion lines without cursor control.
- [ ] Keep all renderer methods side-effect-free except for writing to the supplied handle, and keep raw error strings out of style definitions so error prefixes remain greppable.
- [ ] Add JSON passthrough helpers that write the original validated response bytes rather than decode/re-encode and accidentally change whitespace, number representation, ordering, or unknown fields.
- [ ] Add layout golden tests at narrow, normal, and wide widths for styled dark/light, 16-color, plain Unicode, and plain ASCII profiles.
- [ ] Add assertions that every plain or non-TTY fixture contains no ANSI escapes, hyperlinks, cursor controls, terminal-title sequences, or layout-dependent trailing spaces.

### Task 4: Add safe Huh forms and terminal lifecycle handling

Use Huh only for commands that genuinely need interactive choices or missing values. A form must never make a previously required value implicit, accept a destructive default, or prevent non-interactive automation.

- [ ] Wrap Huh behind an `internal/cliui` form interface with explicit input/output, context cancellation, theme, width, and accessible-mode configuration.
- [ ] Support select, text input, secret input, confirm, and grouped review forms needed by login and the guided installer; keep validation callbacks domain-specific and outside the rendering package.
- [ ] When `secondbox login` is missing one or more required values and input/output are interactive, prompt only for the missing values, mask the token, show its storage target, verify authority, and write credentials exactly as today.
- [ ] Preserve the current fully flagged login path without a form and preserve the current missing-value error for non-TTY invocation.
- [ ] Require an affirmative choice for destructive confirmations; EOF, inaccessible input, escape at the first step, and an empty answer must never mean yes.
- [ ] Map `--accessible` and `SECONDBOX_ACCESSIBLE=1` to Huh's standard-prompt accessible mode and provide the same information and validation as the visual form.
- [ ] Restore terminal state on success, validation failure, returned error, context cancellation, SIGINT, SIGTERM, SIGHUP, and panic boundaries without swallowing the original error.
- [ ] Make Ctrl+C produce the conventional interrupted exit status while retaining the existing cleanup contexts for leases, terminals, and installer stages.
- [ ] Add injected-I/O unit tests and PTY tests for navigation, resize, paste, secret masking, accessible prompts, EOF, cancellation, signals, terminal restoration, and non-interactive refusal.

### Task 5: Migrate `secondbox` read and account commands

Adopt the shared UI first on commands that return bounded data and do not own a guest byte stream. Automatic human rendering may change the TTY experience, but non-TTY output and explicit JSON must remain byte-compatible with Task 1 fixtures.

- [ ] Render `whoami`, login, logout, and version success as concise summaries while retaining exact raw build information for explicit/non-TTY machine use.
- [ ] Add typed table/detail presentation models for bounded list/get/inspect aliases whose OpenAPI responses have stable generated schemas; keep `secondbox operation OPERATION_ID` as the raw escape hatch.
- [ ] Render Sandbox, Snapshot, Artifact, Runner, RunnerPool, Profile, Lease, Port, and Operation states with consistent semantic status treatment and copyable opaque identifiers.
- [ ] Port timing summaries from `text/tabwriter` to the shared table model while keeping the existing plain layout contract available and retaining every measured value.
- [ ] Render resource check/apply reports and diagnostics-bundle completion through shared warnings, phase rows, and receipts without changing their JSON or archive contents.
- [ ] Leave file content, artifact content, logs, terminal negotiation payloads, and any unknown or unbounded API response raw rather than guessing a human schema.
- [ ] Add per-command golden tests, JSON passthrough tests, unknown-field fixtures, narrow-terminal layouts, empty lists, large identifiers, Unicode names, and API problem rendering.

### Task 6: Add progress without contaminating execution streams

Improve long-running lifecycle feedback only around the raw stream, never inside it. Status begins on stderr before guest attachment and ends before guest output is allowed to flow.

- [ ] Model create, scheduling, Runner admission, readiness, exec negotiation, stop/start/drain/delete, snapshot, relocation, and terminal attachment as bounded human status phases derived from existing public operation state.
- [ ] Show inline progress on TTY stderr and bounded plain transitions elsewhere, with polling/retry details collapsed unless a warning or failure needs remediation.
- [ ] Ensure `secondbox run`, `exec`, `shell`, `sandbox shell`, and `exec stream` stop and clear progress before forwarding the first guest stdout/stderr/control byte.
- [ ] Preserve exact guest stdin, stdout, stderr, terminal mode, resize, detach, reconnect, cancellation, cleanup, and exit-status behavior under styled, plain, redirected, and mixed-TTY configurations.
- [ ] Do not create new polling loops or infer lifecycle state inside the UI; consume the existing operation/watch/readiness paths and fail explicitly when required evidence is absent.
- [ ] Render final retained-Sandbox identity, cleanup result, or failure remediation on stderr only when doing so cannot conflict with the guest's error contract.
- [ ] Add concurrency/race tests around progress shutdown and stream start, byte-boundary tests, nonzero guest exits, abrupt disconnects, signals, mixed stdout/stderr TTY states, and slow readiness.

### Task 7: Migrate `secondbox-deploy` human output and prepare the installer UI

Give deployment operations the same visual language while preserving the exact artifacts and stdout values consumed by scripts. This task provides the UI primitives that the separate guided-installer plan uses instead of building its own prompt layer.

- [ ] Replace the single-line usage error with shared structured help on an interactive terminal and retain a stable plain help/error form for non-TTY and tests.
- [ ] Render `validate`, initialization completion, Runner enrollment completion, migration completion, and Compose phase status as summaries or phase lists while leaving their path values copyable.
- [ ] Keep `runner-template` TOML, `verify` JSON, `inspect` JSON, rendered environment files, and all generated manifests byte-identical; add explicit human summary flags where useful instead of changing machine authority.
- [ ] Route Docker Compose subprocess stdout/stderr directly unless the installer invokes the refactored internal Compose boundary with typed progress; never parse cosmetic Docker output as deployment state.
- [ ] Render release verification, asset download/materialization, host preflight, sudo review, service readiness, Runner enrollment, and smoke execution through the shared phase and receipt models required by the guided-installer plan.
- [ ] Supply Huh form builders for workspace choice, capacity review, network/path advanced settings, final install confirmation, resume selection, uninstall, and purge confirmation without moving install authority into UI state.
- [ ] Keep the guided-installer implementation contract on `internal/cliui` forms, PTY tests, and renderer goldens so it does not grow a second terminal abstraction.
- [ ] Add deploy command tests proving raw artifacts and script captures are unchanged, progress remains on stderr, Compose exit codes survive, and installer presentation contains no secrets.

### Task 8: Complete compatibility, accessibility, documentation, and release gates

Close the migration with realistic terminal tests and documentation that makes automatic behavior predictable. Remove replaced formatting only after every command is classified and covered.

- [ ] Add `just test-cli-ui` covering golden views, capability profiles, PTY forms, signals, raw-output fixtures, guest stream boundaries, and both release binaries outside a repository checkout.
- [ ] Run the actual `secondbox` and `secondbox-deploy` binaries under a PTY at multiple sizes and under pipes/redirection to prove renderer selection follows the declared contract.
- [ ] Test terminals with light/dark backgrounds, 1/16/256/true-color profiles, `TERM=dumb`, `NO_COLOR`, ASCII-only locale, accessible mode, CI, stdin redirection, stdout redirection, and stderr redirection.
- [ ] Add fuzz/property tests for width-constrained tables, arbitrary API strings, ANSI/control characters in server-provided values, and error text so untrusted content cannot inject terminal control sequences.
- [ ] Measure release binary size and cold startup before and after the Charm dependencies; record the change and address it only if it violates an explicit release budget.
- [ ] Update README and CLI operations documentation with screenshots or text captures, output/color/accessibility flags, environment behavior, JSON/plain guarantees, and examples safe for scripts.
- [ ] Update third-party notices and release/source-free checks for every direct and transitive UI dependency required by policy.
- [ ] Remove replaced `tabwriter`/ad hoc formatting and duplicated prompt code, but retain explicit raw paths rather than compatibility shims around obsolete presentation functions.
- [ ] Run `just verify-generated`, `just lint`, `just test`, contract/deployment/Compose/SDK/CLI UI suites, `just test-non-kvm`, and `just test-scenario`; hand off raw-stream and PTY evidence with the implementation report.
