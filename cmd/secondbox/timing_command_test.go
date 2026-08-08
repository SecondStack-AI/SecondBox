package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestSandboxTimingCommandUsesBoundedPublishedRouteAndRendersBreakdown(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/v1/sandboxes/sbox_1/timings" ||
			request.URL.Query().Get("limit") != "7" {
			t.Errorf("timing request = %s?%s", request.URL.Path, request.URL.RawQuery)
		}
		if request.Header.Get("Authorization") != "Bearer timing-platform-token" ||
			request.Header.Get("X-SecondBox-Tenant-Ref") != "tenant-1" ||
			request.Header.Get("X-SecondBox-Subject-Ref") != "subject-1" {
			t.Errorf("timing authority headers = %#v", request.Header)
		}
		_ = json.NewEncoder(writer).Encode(secondboxclient.SandboxTiming{
			SandboxID: "sbox_1",
			Operations: []secondboxclient.OperationTiming{{
				OperationID: "op_1", SandboxID: "sbox_1", Kind: "create",
				State: "succeeded", CreatedAt: now,
				QueueMilliseconds:     int64Pointer(12),
				ExecutionMilliseconds: int64Pointer(80),
				TotalMilliseconds:     int64Pointer(92),
				Orchestration: []secondboxclient.OperationStageTiming{{
					Stage: "placement_ready", ObservedAt: now,
					ElapsedMilliseconds: 7.5, CumulativeMilliseconds: 19.5,
				}},
				Boots: []secondboxclient.BootTiming{{
					Generation: 1, DurationMilliseconds: 70, Completed: true,
					Stages: []secondboxclient.BootStageTiming{{
						Stage: "compute_launch", ObservedAt: now, ReceivedAt: now,
						ElapsedMilliseconds: 50, CumulativeMilliseconds: 70,
					}},
				}},
			}},
			Execs: []secondboxclient.ExecTiming{{
				SessionID: "dps_1", Mode: "buffered", Outcome: "exited",
				ElapsedMilliseconds: 9, CreatedAt: now, CompletedAt: now,
			}},
		})
	}))
	defer server.Close()

	var output bytes.Buffer
	err := runTimingCommand(
		context.Background(), server.URL, "timing-platform-token",
		"tenant-1", "subject-1", "sandbox",
		[]string{"--sandbox-id", "sbox_1", "--limit", "7"},
		&output, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"Sandbox: sbox_1",
		"op_1",
		"12ms",
		"Orchestration: operation=op_1",
		"placement_ready",
		"Boot: operation=op_1 generation=1 total=70ms completed=true",
		"compute_launch",
		"dps_1",
		"buffered",
		"9ms",
	} {
		if !strings.Contains(output.String(), fragment) {
			t.Fatalf("timing output does not contain %q:\n%s", fragment, output.String())
		}
	}
}

func TestDeploymentTimingCommandRequiresAndSendsExplicitWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/v1/timings" ||
			request.URL.Query().Get("windowSeconds") != "300" {
			t.Errorf("summary request = %s?%s", request.URL.Path, request.URL.RawQuery)
		}
		_ = json.NewEncoder(writer).Encode(secondboxclient.DeploymentTimingSummary{
			WindowSeconds: 300,
			ObservedAt:    time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
			Boot: secondboxclient.DurationPercentiles{
				Count: 4, P50Milliseconds: float64Pointer(80),
				P95Milliseconds: float64Pointer(120), P99Milliseconds: float64Pointer(120),
			},
			Exec: secondboxclient.DurationPercentiles{},
			API:  secondboxclient.DurationPercentiles{},
		})
	}))
	defer server.Close()

	var output bytes.Buffer
	err := runTimingCommand(
		context.Background(), server.URL, "timing-platform-token",
		"tenant-1", "subject-1", "summary",
		[]string{"--window", "5m"}, &output, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Window: 300s") ||
		!strings.Contains(output.String(), "boot") ||
		!strings.Contains(output.String(), "120ms") {
		t.Fatalf("summary output:\n%s", output.String())
	}

	err = runTimingCommand(
		context.Background(), server.URL, "timing-platform-token",
		"tenant-1", "subject-1", "summary", nil, &output, server.Client(),
	)
	if err == nil || !strings.Contains(err.Error(), "requires --window") {
		t.Fatalf("missing-window error = %v", err)
	}
}

func TestTimingCommandHonorsExplicitJSONOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(secondboxclient.OperationTiming{OperationID: "op_json", SandboxID: "sbox_json", Kind: "start", State: "succeeded"})
	}))
	defer server.Close()
	var output bytes.Buffer
	capabilities := cliui.ForWriter(&output, &output)
	ctx := withPresentation(context.Background(), presentation{renderer: cliui.Renderer{Output: &output, Diagnostic: &output, Capabilities: capabilities, OutputMode: cliui.OutputJSON, ColorMode: cliui.ColorNever}})
	if err := runTimingCommand(ctx, server.URL, "token", "tenant", "subject", "operation", []string{"--operation-id", "op_json"}, &output, server.Client()); err != nil {
		t.Fatal(err)
	}
	var decoded secondboxclient.OperationTiming
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("timing JSON = %q: %v", output.String(), err)
	}
	if decoded.OperationID != "op_json" || strings.Contains(output.String(), "OPERATION") {
		t.Fatalf("timing JSON contract = %q", output.String())
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func float64Pointer(value float64) *float64 {
	return &value
}
