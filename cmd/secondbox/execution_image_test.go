package main

import (
	"context"
	"io"
	"net/http"
)

const testExecutionImageReference = "registry.example/secondbox/test-agent:stable"

func withTestExecutionImage(args []string) []string {
	if len(args) == 0 {
		return args
	}
	result := make([]string, 0, len(args)+2)
	result = append(result, args[0], "--image", testExecutionImageReference)
	return append(result, args[1:]...)
}

func runTestLifecycleVerb(ctx context.Context, session cliSession, command string, args []string, output io.Writer, transport *http.Client) error {
	if command == "create" || command == "start" {
		args = withTestExecutionImage(args)
	}
	return runLifecycleVerb(ctx, session, command, args, output, transport)
}

func runTestRunCommand(ctx context.Context, session cliSession, args []string, environment execCommandEnvironment, terminal sandboxShellEnvironment) error {
	return runRunCommand(ctx, session, withTestExecutionImage(args), environment, terminal)
}
