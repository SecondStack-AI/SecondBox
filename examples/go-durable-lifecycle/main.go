package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	secondbox "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func main() {
	client, err := secondbox.NewSecondBoxSubjectClient(
		mustEnv("SECONDBOX_URL"), mustEnv("SECONDBOX_TOKEN"),
		mustEnv("SECONDBOX_TENANT_REF"), mustEnv("SECONDBOX_SUBJECT_REF"),
		http.DefaultClient,
	)
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := client.ValidateProfile(ctx, "durable-coding"); err != nil {
		panic(err)
	}
	handle, _, err := client.CreateSandbox(ctx, secondbox.CreateSandboxRequest{
		Profile: "durable-coding", Metadata: secondbox.Metadata{"example": "go"},
	}, "")
	if err != nil {
		panic(err)
	}
	if _, err := handle.WaitFor(ctx, secondbox.SandboxStateReady); err != nil {
		panic(err)
	}
	outcome, err := handle.Execute(ctx, secondbox.BufferedExecRequest{
		Command:     secondbox.Command{ShellCommand: &secondbox.ShellCommand{Mode: "shell", Command: "printf durable"}},
		Environment: secondbox.StringMap{}, DeadlineMilliseconds: 30_000, MaximumOutputBytes: 1 << 20,
	}, "", "")
	if err != nil {
		panic(err)
	}
	result, err := secondbox.DecodeExecOutcome(outcome)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Sandbox %s said %s\n", handle.Snapshot().ID, result.Stdout)
	if _, err := handle.Stop(ctx, secondbox.LifecycleOptions{}); err != nil {
		panic(err)
	}
	// Stop retains the durable Sandbox and Workspace. Deletion is an explicit,
	// separate application decision.
}

func mustEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		panic(name + " is required")
	}
	return value
}
