// secondbox-stale-runner-probe submits one authenticated stale Assignment result for qualification.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

type staleAssignmentProbeInput struct {
	AssignmentID string `json:"assignmentId"`
	SandboxID    string `json:"sandboxId"`
	InstanceID   string `json:"instanceId"`
	Generation   uint64 `json:"generation"`
	FencingToken string `json:"fencingTokenBase64"`
	RequestID    string `json:"requestId"`
	OperationID  string `json:"operationId"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("secondbox-stale-runner-probe", flag.ContinueOnError)
	inputPath := flags.String("input", "", "stale Assignment probe JSON")
	timeoutText := flags.String("timeout", "", "bounded probe timeout")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*inputPath) == "" ||
		strings.TrimSpace(*timeoutText) == "" {
		return errors.New("SecondBox stale Runner probe requires --input and --timeout")
	}
	timeout, err := time.ParseDuration(*timeoutText)
	if err != nil || timeout <= 0 || timeout > time.Minute {
		return errors.New("SecondBox stale Runner probe timeout must be positive and at most one minute")
	}
	input, err := loadStaleAssignmentProbeInput(*inputPath)
	if err != nil {
		return err
	}
	protocolConfig, connectorConfig, err := runnercontrol.LoadRunnerProtocolConfigFromEnv()
	if err != nil {
		return err
	}
	connector, err := runnercontrol.NewGRPCConnector(connectorConfig)
	if err != nil {
		return err
	}
	defer connector.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stream, err := connector.Connect(ctx)
	if err != nil {
		return err
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("SecondBox stale Runner probe nonce: %w", err)
	}
	if err := stream.Send(&runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Hello{
			Hello: &runnerprotocol.RunnerHello{
				RunnerId: protocolConfig.RunnerID, ConnectionNonce: nonce,
				SupportedVersions: &runnerprotocol.ProtocolVersionRange{
					Minimum: protocolConfig.ProtocolMinimum,
					Maximum: protocolConfig.ProtocolMaximum,
				},
				RequestedFeatures: protocolConfig.MandatoryFeatures,
				MandatoryFeatures: protocolConfig.MandatoryFeatures,
			},
		},
	}); err != nil {
		return fmt.Errorf("SecondBox stale Runner probe send Hello: %w", err)
	}
	welcomeFrame, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("SecondBox stale Runner probe receive Welcome: %w", err)
	}
	welcome := welcomeFrame.GetWelcome()
	if welcome == nil || welcome.ConnectionId == "" {
		return errors.New("SecondBox stale Runner probe did not negotiate a connection")
	}
	if err := stream.Send(staleProbeRegistration(protocolConfig, welcome.ConnectionId)); err != nil {
		return fmt.Errorf("SecondBox stale Runner probe send Registration: %w", err)
	}
	if err := stream.Send(staleProbeDrainedHeartbeat(protocolConfig.RunnerID, welcome.ConnectionId)); err != nil {
		return fmt.Errorf("SecondBox stale Runner probe send drained Heartbeat: %w", err)
	}
	if err := stream.Send(staleProbeAssignmentResult(protocolConfig.RunnerID, input)); err != nil {
		return fmt.Errorf("SecondBox stale Runner probe send Assignment result: %w", err)
	}
	for {
		_, receiveErr := stream.Recv()
		if receiveErr == nil {
			continue
		}
		if strings.Contains(receiveErr.Error(), "stale assignment fencing") {
			fmt.Println("SecondBox stale Runner probe observed generation-fenced rejection")
			return nil
		}
		return fmt.Errorf("SecondBox stale Runner probe received a non-fencing failure: %w", receiveErr)
	}
}

func loadStaleAssignmentProbeInput(path string) (staleAssignmentProbeInput, error) {
	var input staleAssignmentProbeInput
	file, err := os.Open(path)
	if err != nil {
		return input, fmt.Errorf("SecondBox stale Runner probe input: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("SecondBox stale Runner probe input decode: %w", err)
	}
	token, err := base64.StdEncoding.DecodeString(input.FencingToken)
	if err != nil || len(token) < 32 {
		return input, errors.New("SecondBox stale Runner probe fencing token is invalid")
	}
	if input.AssignmentID == "" || input.SandboxID == "" || input.InstanceID == "" ||
		input.Generation == 0 || input.RequestID == "" || input.OperationID == "" {
		return input, errors.New("SecondBox stale Runner probe Assignment authority is incomplete")
	}
	return input, nil
}

func staleProbeRegistration(
	config runnercontrol.RunnerProtocolConfig,
	connectionID string,
) *runnerprotocol.RunnerToControlPlane {
	return &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Registration{
			Registration: &runnerprotocol.RunnerRegistration{
				MessageId: "qualification-stale-registration", Sequence: 1,
				RunnerId: config.RunnerID, ConnectionId: connectionID,
				RunnerPoolId:    config.RunnerPoolID,
				SoftwareVersion: config.SoftwareVersion + "-qualification-stale-probe",
				ProtocolVersion: config.ProtocolMaximum,
				Capabilities: &runnerprotocol.RunnerCapabilities{
					Architecture: "amd64", HypervisorReady: true, IsolationReady: true,
					ResourceLimitsReady: true, NetworkPolicyReady: true, StorageReady: true,
					CleanupReady: true, ComputeBackendVersion: "qualification-stale-probe",
					GuestProtocolGenerations: &runnerprotocol.ProtocolVersionRange{
						Minimum: 1, Maximum: 1,
					},
				},
				Allocatable: &runnerprotocol.Capacity{
					VcpuCount: 1, MemoryBytes: 1, DiskBytes: 1, Instances: 1, Operations: 1,
				},
				Reserved:      &runnerprotocol.Capacity{},
				StartupTiming: &runnerprotocol.StartupTiming{},
			},
		},
	}
}

func staleProbeDrainedHeartbeat(
	runnerID string,
	connectionID string,
) *runnerprotocol.RunnerToControlPlane {
	return &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_Heartbeat{
			Heartbeat: &runnerprotocol.RunnerHeartbeat{
				MessageId: "qualification-stale-drained-heartbeat", Sequence: 2,
				RunnerId: runnerID, ConnectionId: connectionID,
				ObservedAtUnixMs: uint64(time.Now().UTC().UnixMilli()),
				Allocatable:      &runnerprotocol.Capacity{},
				Reserved:         &runnerprotocol.Capacity{},
				DrainPhase:       runnerprotocol.DrainPhase_DRAIN_PHASE_DRAINED,
				StartupTiming:    &runnerprotocol.StartupTiming{},
			},
		},
	}
}

func staleProbeAssignmentResult(
	runnerID string,
	input staleAssignmentProbeInput,
) *runnerprotocol.RunnerToControlPlane {
	token, _ := base64.StdEncoding.DecodeString(input.FencingToken)
	fence := &runnerprotocol.AssignmentFence{
		AssignmentId: input.AssignmentID, SandboxId: input.SandboxID,
		InstanceId: input.InstanceID, SandboxGeneration: input.Generation,
		FencingToken: token,
	}
	return &runnerprotocol.RunnerToControlPlane{
		Message: &runnerprotocol.RunnerToControlPlane_AssignmentResult{
			AssignmentResult: &runnerprotocol.AssignmentResult{
				MessageId: "qualification-stale-assignment-result", Sequence: 3,
				Fence:       fence,
				Terminal:    runnerprotocol.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
				BackendKind: "firecracker", BackendReference: "qualification-stale-backend",
				Correlation: &runnerprotocol.Correlation{
					RequestId: input.RequestID, OperationId: input.OperationID,
					SandboxId: input.SandboxID, InstanceId: input.InstanceID,
					SandboxGeneration: input.Generation, AssignmentId: input.AssignmentID,
					RunnerId: runnerID,
				},
			},
		},
	}
}
