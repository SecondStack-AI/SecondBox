package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const (
	defaultExecDeadline    = time.Minute
	defaultExecOutputBytes = 1 << 20
)

// commandExitError carries a guest command's own exit status to the CLI's status.
//
// The guest already wrote its diagnosis to standard error, so this error is
// reported by exit status alone rather than by an additional message.
type commandExitError struct {
	code int
}

func (failure *commandExitError) Error() string {
	return fmt.Sprintf("SecondBox remote command exited with status %d", failure.code)
}

type execCommandEnvironment struct {
	stdout     io.Writer
	stderr     io.Writer
	httpClient *http.Client
}

func runExecCommand(
	ctx context.Context,
	session cliSession,
	args []string,
	environment execCommandEnvironment,
) error {
	sandboxReference, rest, err := splitLeadingOperand("exec", "Sandbox", args)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	shell := flags.Bool("shell", false, "treat the single operand as one shell command")
	cwd := flags.String("cwd", "", "workspace-relative working directory")
	var environmentValues repeatedValues
	flags.Var(&environmentValues, "env", "environment name=value; repeatable")
	deadline := flags.Duration("deadline", defaultExecDeadline, "command deadline")
	maximumOutputBytes := flags.Int64(
		"max-output-bytes", defaultExecOutputBytes, "maximum buffered output bytes",
	)
	leaseID := flags.String("lease", "", "optional Lease ID")
	idempotencyKey := flags.String("idempotency-key", "", "optional request idempotency key")
	emitJSON := flags.Bool("json", false, "write the raw ExecOutcome JSON instead of the output")
	if err := flags.Parse(rest); err != nil {
		return fmt.Errorf("SecondBox CLI parse exec options: %w", err)
	}
	if err := requireSessionCredentials("exec", session); err != nil {
		return err
	}
	if *deadline < time.Millisecond {
		return errors.New("SecondBox CLI exec --deadline must be at least one millisecond")
	}
	if *maximumOutputBytes < 1 {
		return errors.New("SecondBox CLI exec --max-output-bytes must be positive")
	}
	command, err := buildExecCommand(*shell, flags.Args())
	if err != nil {
		return err
	}
	values, err := parsePairs(environmentValues)
	if err != nil {
		return fmt.Errorf("SecondBox CLI exec environment: %w", err)
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
	handle, err := resolveSandboxReference(ctx, client, sandboxReference)
	if err != nil {
		return err
	}
	request := secondboxclient.BufferedExecRequest{
		Command:              command,
		Environment:          secondboxclient.StringMap(values),
		DeadlineMilliseconds: deadline.Milliseconds(),
		MaximumOutputBytes:   *maximumOutputBytes,
	}
	if *cwd != "" {
		workspacePath := secondboxclient.WorkspacePath(*cwd)
		request.Cwd = &workspacePath
	}
	outcome, err := handle.Execute(ctx, request, *idempotencyKey, *leaseID)
	if err != nil {
		return err
	}
	if *emitJSON {
		return writeExecOutcomeJSON(environment.stdout, outcome)
	}
	return writeExecOutcome(environment, outcome)
}

// writeExecOutcome writes the decoded streams before reporting the status, so a
// failing command still explains itself.
func writeExecOutcome(
	environment execCommandEnvironment,
	outcome secondboxclient.ExecOutcome,
) error {
	result, outcomeErr := secondboxclient.DecodeExecOutcome(outcome)
	if _, err := environment.stdout.Write(result.Stdout); err != nil {
		return fmt.Errorf("SecondBox CLI write exec stdout: %w", err)
	}
	if _, err := environment.stderr.Write(result.Stderr); err != nil {
		return fmt.Errorf("SecondBox CLI write exec stderr: %w", err)
	}
	return execOutcomeStatus(outcome, outcomeErr)
}

func writeExecOutcomeJSON(output io.Writer, outcome secondboxclient.ExecOutcome) error {
	encoded, err := json.Marshal(outcome)
	if err != nil {
		return fmt.Errorf("SecondBox CLI encode exec outcome: %w", err)
	}
	if _, err := output.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("SecondBox CLI write exec outcome: %w", err)
	}
	return execOutcomeStatus(outcome, secondboxclient.ExecOutcomeError(outcome))
}

// execOutcomeStatus reports an exited command through the CLI's own exit status
// and every other terminal outcome as a described failure.
func execOutcomeStatus(outcome secondboxclient.ExecOutcome, outcomeErr error) error {
	if outcomeErr == nil {
		return nil
	}
	if outcome.ExecExited != nil {
		return &commandExitError{code: outcome.ExecExited.ExitCode}
	}
	return fmt.Errorf("SecondBox remote command: %w", outcomeErr)
}

func buildExecCommand(shell bool, operands []string) (secondboxclient.Command, error) {
	if len(operands) == 0 {
		return secondboxclient.Command{}, errors.New(
			"SecondBox CLI exec requires a command after --",
		)
	}
	if shell {
		if len(operands) != 1 {
			return secondboxclient.Command{}, errors.New(
				"SecondBox CLI exec --shell requires exactly one command operand",
			)
		}
		return secondboxclient.Command{ShellCommand: &secondboxclient.ShellCommand{
			Mode: "shell", Command: operands[0],
		}}, nil
	}
	return secondboxclient.Command{ArgvCommand: &secondboxclient.ArgvCommand{
		Mode: "argv", Executable: operands[0], Arguments: operands[1:],
	}}, nil
}

// resolveSandboxHandle retrieves the current Sandbox so its generation is
// applied automatically rather than supplied by the caller.
func resolveSandboxHandle(
	ctx context.Context,
	client *secondboxclient.Client,
	reference string,
) (*secondboxclient.SandboxHandle, error) {
	var sandbox secondboxclient.Sandbox
	err := client.RequestJSON(ctx, "getSandbox", secondboxclient.CallOptions{
		PathParameters: map[string]string{"sandboxId": reference},
	}, &sandbox)
	if err != nil {
		return nil, err
	}
	if sandbox.ID == "" {
		return nil, fmt.Errorf("SecondBox CLI received no Sandbox for %q", reference)
	}
	return secondboxclient.NewSandboxHandle(client, sandbox), nil
}

// splitLeadingOperand takes the required operand that precedes every option, so
// `exec my-box --deadline 5s -- printf hello` reads in its natural order.
func splitLeadingOperand(
	command string,
	operand string,
	args []string,
) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("SecondBox CLI %s requires a %s", command, operand)
	}
	if strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf(
			"SecondBox CLI %s requires the %s before any option", command, operand,
		)
	}
	return args[0], args[1:], nil
}

func requireSessionCredentials(command string, session cliSession) error {
	if strings.TrimSpace(session.url) == "" || strings.TrimSpace(session.token) == "" ||
		strings.TrimSpace(session.tenantRef) == "" || strings.TrimSpace(session.subjectRef) == "" {
		return fmt.Errorf(
			"SecondBox CLI %s requires --url, --token, --tenant-ref, and --subject-ref%s",
			command, sessionSourceHint,
		)
	}
	return nil
}
