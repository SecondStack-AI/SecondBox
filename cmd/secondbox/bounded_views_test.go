package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
)

func TestEveryBoundedOperationHasPlainNarrowView(t *testing.T) {
	fixtures := map[string]string{
		"listProfiles": `{"items":[],"nextCursor":"cursor-next"}`, "listRunnerPools": `{"items":[]}`,
		"listRunners": `{"items":[]}`, "listSandboxes": `{"items":[]}`,
		"listSandboxSnapshots": `{"items":[]}`, "listSandboxArtifacts": `{"items":[]}`,
		"getProfile":            `{"name":"プロファイル","state":"enabled","currentRevision":{"number":1},"revision":1}`,
		"getRunnerPool":         `{"name":"pool","state":"ready","architectures":["amd64"],"revision":1}`,
		"getRunner":             `{"id":"run_abcdefghijklmnopqrstuvwxyz0123456789","name":"runner","poolName":"pool","state":"ready","credentialState":"active","revision":1}`,
		"getSandbox":            `{"id":"sbx_abcdefghijklmnopqrstuvwxyz0123456789","profile":"durable-coding","state":"ready","desiredState":"running","generation":1,"revision":1}`,
		"getSnapshot":           `{"id":"snp_1","name":"snapshot","sandboxId":"sbx_1","state":"ready","generation":1,"sizeBytes":1}`,
		"getArtifact":           `{"id":"art_1","name":"artifact","sandboxId":"sbx_1","mediaType":"application/octet-stream","sizeBytes":1,"sha256":"sha256"}`,
		"getSandboxLease":       `{"id":"lea_1","sandboxId":"sbx_1","state":"active","generation":1,"expiresAt":"2026-08-07T00:00:00Z"}`,
		"getSandboxPortSession": `{"id":"por_1","name":"https","sandboxId":"sbx_1","state":"open","protocol":"https","endpoint":"127.0.0.1:443"}`,
		"inspectSandbox":        `{"sandboxId":"sbx_1","generation":1,"guestHealthy":true,"activeSessions":0,"observedAt":"2026-08-07T00:00:00Z"}`,
		"getOperation":          `{"id":"op_1","kind":"create","state":"succeeded","sandboxId":"sbx_1","requestId":"req_1"}`,
	}
	if len(fixtures) != len(boundedOperations) {
		t.Fatalf("fixtures = %d, bounded operations = %d", len(fixtures), len(boundedOperations))
	}
	for operation, content := range fixtures {
		t.Run(operation, func(t *testing.T) {
			var output bytes.Buffer
			capabilities := cliui.ForWriter(&output, io.Discard)
			capabilities.Output.Width = 36
			renderer := cliui.Renderer{Output: &output, Diagnostic: io.Discard, Capabilities: capabilities, OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
			if err := renderBoundedOperation(operation, []byte(content), renderer); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), " \n") {
				t.Fatalf("unsafe view: %q", output.String())
			}
			if operation == "listProfiles" && !strings.Contains(output.String(), "Next cursor: cursor-next") {
				t.Fatalf("continuation cursor was dropped: %q", output.String())
			}
		})
	}
}
