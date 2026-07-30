package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const defaultRunReadyTimeout = 5 * time.Minute

func runRunCommand(
	ctx context.Context,
	session cliSession,
	args []string,
	environment execCommandEnvironment,
) (resultErr error) {
	profile, rest, err := splitLeadingOperand("run", "Profile", args)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "reserved Sandbox name for later reference")
	keep := flags.Bool("keep", false, "retain the Sandbox instead of deleting it")
	shell := flags.Bool("shell", false, "treat the single operand as one shell command")
	cwd := flags.String("cwd", "", "workspace-relative working directory")
	var environmentValues repeatedValues
	flags.Var(&environmentValues, "env", "environment name=value; repeatable")
	var metadataValues repeatedValues
	flags.Var(&metadataValues, "metadata", "Sandbox metadata name=value; repeatable")
	deadline := flags.Duration("deadline", defaultExecDeadline, "command deadline")
	maximumOutputBytes := flags.Int64(
		"max-output-bytes", defaultExecOutputBytes, "maximum buffered output bytes",
	)
	readyTimeout := flags.Duration(
		"ready-timeout", defaultRunReadyTimeout, "time allowed for the Sandbox to become ready",
	)
	forwardStdin := flags.Bool("stdin", false, "send standard input to the command")
	emitJSON := flags.Bool("json", false, "write the raw ExecOutcome JSON instead of the output")
	if err := flags.Parse(rest); err != nil {
		return fmt.Errorf("SecondBox CLI parse run options: %w", err)
	}
	if err := requireSessionCredentials("run", session); err != nil {
		return err
	}
	if *deadline < time.Millisecond {
		return errors.New("SecondBox CLI run --deadline must be at least one millisecond")
	}
	if *maximumOutputBytes < 1 {
		return errors.New("SecondBox CLI run --max-output-bytes must be positive")
	}
	if *readyTimeout < time.Second {
		return errors.New("SecondBox CLI run --ready-timeout must be at least one second")
	}
	command, err := buildExecCommand(*shell, flags.Args())
	if err != nil {
		return err
	}
	values, err := parsePairs(environmentValues)
	if err != nil {
		return fmt.Errorf("SecondBox CLI run environment: %w", err)
	}
	metadata, err := parsePairs(metadataValues)
	if err != nil {
		return fmt.Errorf("SecondBox CLI run metadata: %w", err)
	}
	if *name != "" {
		if _, reserved := metadata[contracts.SandboxNameMetadataKey]; reserved {
			return fmt.Errorf(
				"SecondBox CLI run cannot combine --name with metadata %s",
				contracts.SandboxNameMetadataKey,
			)
		}
		metadata[contracts.SandboxNameMetadataKey] = *name
	}
	if environment.httpClient == nil {
		environment.httpClient = http.DefaultClient
	}
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		session.url, session.token, session.tenantRef, session.subjectRef, environment.httpClient,
	)
	if err != nil {
		return err
	}
	request := secondboxclient.RunRequest{
		Profile:              profile,
		Metadata:             metadata,
		Command:              command,
		Environment:          secondboxclient.StringMap(values),
		DeadlineMilliseconds: deadline.Milliseconds(),
		MaximumOutputBytes:   *maximumOutputBytes,
	}
	if *cwd != "" {
		workspacePath := secondboxclient.WorkspacePath(*cwd)
		request.Cwd = &workspacePath
	}
	// Read standard input before creating anything, so an oversized input fails
	// without leaving a Sandbox behind.
	if *forwardStdin {
		stdin, err := readExecStdin("run", environment.stdin)
		if err != nil {
			return err
		}
		request.StdinBase64 = stdin
	}
	// The SDK requires a deadline covering both becoming ready and running.
	runContext, cancel := context.WithTimeout(ctx, *readyTimeout+*deadline)
	defer cancel()
	handle, result, runErr := client.Run(runContext, request)
	if handle != nil && !*keep {
		defer func() {
			resultErr = errors.Join(resultErr, deleteRunSandbox(ctx, handle))
		}()
	}
	if handle != nil && *keep {
		if _, err := fmt.Fprintf(
			environment.stderr, "SecondBox retained Sandbox %s\n", handle.Snapshot().ID,
		); err != nil {
			return err
		}
	}
	// A transport or lifecycle failure has no outcome to render.
	var failure *secondboxclient.ExecFailure
	if runErr != nil && !errors.As(runErr, &failure) {
		return runErr
	}
	if *emitJSON {
		return writeExecOutcomeJSON(environment.stdout, result.Outcome)
	}
	return writeExecOutcome(environment, result.Outcome)
}

// deleteRunSandbox disposes of the Sandbox this command created. The SDK never
// deletes implicitly, so disposal is requested here and only here.
// runDisposeAttempts bounds the optimistic-concurrency retry below.
const runDisposeAttempts = 5

func deleteRunSandbox(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
) error {
	disposeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	var lastErr error
	for range runDisposeAttempts {
		sandbox, err := handle.Refresh(disposeContext)
		if err != nil {
			return fmt.Errorf("SecondBox CLI run refresh before delete: %w", err)
		}
		if !liveSandbox(sandbox) {
			return nil
		}
		_, err = handle.Delete(disposeContext, secondboxclient.LifecycleOptions{
			IfMatch: secondboxclient.RevisionETag(sandbox.Revision),
		})
		if err == nil {
			return nil
		}
		// Reconciliation advances the revision while a Sandbox is managed, so a
		// validator read a moment earlier can already be stale. That is the one
		// failure worth re-reading for; everything else is reported as it is.
		if secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodePreconditionFailed {
			return fmt.Errorf("SecondBox CLI run delete Sandbox %s: %w", sandbox.ID, err)
		}
		lastErr = err
	}
	return fmt.Errorf(
		"SecondBox CLI run delete Sandbox %s after %d attempts: %w",
		handle.Snapshot().ID, runDisposeAttempts, lastErr,
	)
}

func runShellCommand(
	ctx context.Context,
	session cliSession,
	args []string,
	environment sandboxShellEnvironment,
	httpClient *http.Client,
) (resultErr error) {
	reference, rest, err := splitLeadingOperand("shell", "Sandbox", args)
	if err != nil {
		return err
	}
	if err := requireSessionCredentials("shell", session); err != nil {
		return err
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		session.url, session.token, session.tenantRef, session.subjectRef, httpClient,
	)
	if err != nil {
		return err
	}
	handle, err := resolveSandboxReference(ctx, client, reference)
	if err != nil {
		return err
	}
	sandbox := handle.Snapshot()
	// Injected values precede the caller's own arguments, and the flag package
	// keeps the last occurrence, so every injected value stays overridable.
	injected := []string{
		"--sandbox", sandbox.ID,
		"--generation", fmt.Sprintf("%d", sandbox.Generation),
	}
	if !suppliedFlag(rest, "lease") && !suppliedFlag(rest, "session") {
		keeper, err := handle.KeepLease(ctx, shellLeaseDuration)
		if err != nil {
			return err
		}
		defer func() {
			resultErr = errors.Join(resultErr, keeper.Close())
		}()
		injected = append(injected, "--lease", keeper.ID())
	}
	if !suppliedFlag(rest, "idempotency-key") && !suppliedFlag(rest, "session") {
		key, err := secondboxclient.NewIdempotencyKey()
		if err != nil {
			return err
		}
		injected = append(injected, "--idempotency-key", key)
	}
	if environment.httpClient == nil {
		environment.httpClient = httpClient
	}
	return runSandboxShellCommand(
		ctx, session.url, session.token, session.tenantRef, session.subjectRef,
		append(injected, rest...), environment,
	)
}

const shellLeaseDuration = 5 * time.Minute

// suppliedFlag reports whether the caller already gave a flag, so an injected
// default is not acquired needlessly.
func suppliedFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == "-"+name || arg == "--"+name ||
			strings.HasPrefix(arg, "-"+name+"=") || strings.HasPrefix(arg, "--"+name+"=") {
			return true
		}
	}
	return false
}
