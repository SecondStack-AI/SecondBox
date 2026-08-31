package protocol_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type versionWindow struct {
	Minimum uint32 `json:"minimum"`
	Maximum uint32 `json:"maximum"`
}

type runnerNegotiationCase struct {
	Name               string   `json:"name"`
	ClientMinimum      uint32   `json:"client_minimum"`
	ClientMaximum      uint32   `json:"client_maximum"`
	MandatoryFeatures  []string `json:"mandatory_features"`
	ServerFeatures     []string `json:"server_features"`
	SelectedGeneration uint32   `json:"selected_generation"`
	Rejection          string   `json:"rejection"`
}

type messageIdentityCase struct {
	Name                 string `json:"name"`
	PreviousMessageID    string `json:"previous_message_id"`
	PreviousSequence     uint64 `json:"previous_sequence"`
	PreviousFencingToken string `json:"previous_fencing_token"`
	IncomingMessageID    string `json:"incoming_message_id"`
	IncomingSequence     uint64 `json:"incoming_sequence"`
	IncomingFencingToken string `json:"incoming_fencing_token"`
	Outcome              string `json:"outcome"`
}

type fencedMessage struct {
	Kind              string `json:"kind"`
	AssignmentID      string `json:"assignment_id"`
	SandboxGeneration uint64 `json:"sandbox_generation"`
	FencingToken      string `json:"fencing_token"`
	Sequence          uint64 `json:"sequence"`
	Valid             bool   `json:"valid"`
}

type runnerCases struct {
	SchemaGeneration     uint32                  `json:"schema_generation"`
	ServerWindow         versionWindow           `json:"server_window"`
	NegotiationCases     []runnerNegotiationCase `json:"negotiation_cases"`
	MessageIdentityCases []messageIdentityCase   `json:"message_identity_cases"`
	FencedMessages       []fencedMessage         `json:"fenced_messages"`
}

type guestHandshakeCase struct {
	Name               string   `json:"name"`
	GuestMinimum       uint32   `json:"guest_minimum"`
	GuestMaximum       uint32   `json:"guest_maximum"`
	MandatoryFeatures  []string `json:"mandatory_features"`
	GuestFeatures      []string `json:"guest_features"`
	BindingMatches     bool     `json:"binding_matches"`
	SelectedGeneration uint32   `json:"selected_generation"`
	Rejection          string   `json:"rejection"`
}

type frameIdentityCase struct {
	Name              string `json:"name"`
	ConnectionNonce   string `json:"connection_nonce"`
	SandboxGeneration uint64 `json:"sandbox_generation"`
	OperationID       string `json:"operation_id"`
	StreamID          string `json:"stream_id"`
	PreviousSequence  uint64 `json:"previous_sequence"`
	IncomingSequence  uint64 `json:"incoming_sequence"`
	Outcome           string `json:"outcome"`
}

type guestCases struct {
	SchemaGeneration uint32               `json:"schema_generation"`
	HostWindow       versionWindow        `json:"host_window"`
	HandshakeCases   []guestHandshakeCase `json:"handshake_cases"`
	FrameCases       []frameIdentityCase  `json:"frame_identity_cases"`
	TerminalOutcomes []string             `json:"terminal_outcomes"`
}

func TestRunnerProtocolCompatibilityWindow(t *testing.T) {
	cases := readFixture[runnerCases](t, "contracts/runner/v1/fixtures/protocol_cases.json")

	if cases.SchemaGeneration != 4 {
		t.Fatalf("runner fixture must record generation 4, got %d", cases.SchemaGeneration)
	}
	if cases.ServerWindow.Minimum != 4 || cases.ServerWindow.Maximum != 4 {
		t.Fatalf("runner compatibility window = %#v, want generation 4 exactly", cases.ServerWindow)
	}

	for _, tc := range cases.NegotiationCases {
		t.Run(tc.Name, func(t *testing.T) {
			selected, rejection := negotiate(
				cases.ServerWindow,
				versionWindow{Minimum: tc.ClientMinimum, Maximum: tc.ClientMaximum},
				tc.MandatoryFeatures,
				tc.ServerFeatures,
			)
			if selected != tc.SelectedGeneration || rejection != tc.Rejection {
				t.Fatalf("negotiate() = (%d, %q), want (%d, %q)", selected, rejection, tc.SelectedGeneration, tc.Rejection)
			}
		})
	}
}

func TestRunnerMessageIdentityAndFencing(t *testing.T) {
	cases := readFixture[runnerCases](t, "contracts/runner/v1/fixtures/protocol_cases.json")

	for _, tc := range cases.MessageIdentityCases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := classifyRunnerMessage(tc); got != tc.Outcome {
				t.Fatalf("classifyRunnerMessage() = %q, want %q", got, tc.Outcome)
			}
		})
	}

	for _, message := range cases.FencedMessages {
		t.Run("required_fields_"+message.Kind, func(t *testing.T) {
			valid := message.AssignmentID != "" &&
				message.SandboxGeneration != 0 &&
				message.FencingToken != "" &&
				message.Sequence != 0
			if valid != message.Valid {
				t.Fatalf("required field validation = %t, want %t", valid, message.Valid)
			}
		})
	}
}

func TestGuestProtocolCompatibilityAndFeatureNegotiation(t *testing.T) {
	cases := readFixture[guestCases](t, "contracts/guest/v1/fixtures/protocol_cases.json")

	if cases.SchemaGeneration != 1 {
		t.Fatalf("guest fixture must remain frozen at generation 1, got %d", cases.SchemaGeneration)
	}
	if width := cases.HostWindow.Maximum - cases.HostWindow.Minimum + 1; width > 3 {
		t.Fatalf("guest compatibility window retains at most current and two prior generations, got width %d", width)
	}

	for _, tc := range cases.HandshakeCases {
		t.Run(tc.Name, func(t *testing.T) {
			selected, rejection := negotiate(
				cases.HostWindow,
				versionWindow{Minimum: tc.GuestMinimum, Maximum: tc.GuestMaximum},
				tc.MandatoryFeatures,
				tc.GuestFeatures,
			)
			if rejection == "" && !tc.BindingMatches {
				selected = 0
				rejection = "binding_mismatch"
			}
			if selected != tc.SelectedGeneration || rejection != tc.Rejection {
				t.Fatalf("handshake = (%d, %q), want (%d, %q)", selected, rejection, tc.SelectedGeneration, tc.Rejection)
			}
		})
	}
}

func TestGuestFramesAreConnectionAndGenerationBound(t *testing.T) {
	cases := readFixture[guestCases](t, "contracts/guest/v1/fixtures/protocol_cases.json")

	for _, tc := range cases.FrameCases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := classifyGuestFrame(tc, "nonce-a", 4); got != tc.Outcome {
				t.Fatalf("classifyGuestFrame() = %q, want %q", got, tc.Outcome)
			}
			if tc.OperationID == "" || tc.StreamID == "" || tc.IncomingSequence == 0 {
				t.Fatal("frozen frame is missing operation, stream, or sequence identity")
			}
		})
	}

	wantTerminalOutcomes := []string{
		"exited",
		"spawn_failed",
		"deadline_exceeded",
		"cancelled",
		"output_exhausted",
	}
	if !slices.Equal(cases.TerminalOutcomes, wantTerminalOutcomes) {
		t.Fatalf("terminal outcomes = %v, want %v", cases.TerminalOutcomes, wantTerminalOutcomes)
	}
}

func TestCanonicalSchemasExposeRequiredProtocolSurfaces(t *testing.T) {
	testSchema(t, "contracts/runner/v1/runner.proto", []string{
		"service RunnerControl",
		"rpc Connect(stream RunnerToControlPlane) returns (stream ControlPlaneToRunner)",
		"message RunnerRegistration",
		"message RunnerHeartbeat",
		"message Capacity",
		"message AssignmentCommand",
		"message AssignmentAck",
		"message AssignmentProgress",
		"message AssignmentResult",
		"message FenceCommand",
		"message DrainCommand",
		"message Evidence",
		"message AssignmentFence",
		"uint64 sandbox_generation",
		"bytes fencing_token",
		"message ExecFrame",
		"message FileFrame",
		"message PtyFrame",
		"message PortFrame",
		"RUNNER_FEATURE_LOCAL_WORKSPACE",
		"RUNNER_FEATURE_TENANT_EGRESS_CONTEXT",
		"message LocalWorkspaceCommand",
		"message LocalWorkspaceResult",
		"message WorkspaceTransferFrame",
		"LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_FINALIZE",
		"LOCAL_WORKSPACE_TERMINAL_KIND_CONFLICTING_REPLAY",
		"uint64 sequence",
	})
	testSchema(t, "contracts/guest/v1/guest.proto", []string{
		"service GuestAgent",
		"rpc Connect(stream RunnerToGuest) returns (stream GuestToRunner)",
		"message ConnectionBinding",
		"uint64 sandbox_generation",
		"bytes connection_nonce",
		"message Hello",
		"message Welcome",
		"repeated GuestFeature mandatory_features",
		"message Heartbeat",
		"message UsefulActivity",
		"message OperationBinding",
		"message ExecFrame",
		"message FileFrame",
		"message PtyFrame",
		"message PortFrame",
		"message ExecTerminal",
		"EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED",
		"uint64 sequence",
	})
}

func TestWorkspaceRelocationTransferIsTheOnlyWorkspaceImageByteProtocol(t *testing.T) {
	schema := string(readRepoFile(t, "contracts/runner/v1/runner.proto"))
	start := strings.Index(schema, "message WorkspaceTransferOpen {")
	end := strings.Index(schema, "// InstanceObservedTerminationReason")
	if start == -1 || end == -1 || end <= start {
		t.Fatal("Workspace relocation transfer protocol section is missing")
	}
	section := schema[start:end]
	for _, required := range []string{
		"bytes data",
		"StreamCredit credit",
		"string sha256",
		"bytes fencing_token",
	} {
		if !strings.Contains(section, required) {
			t.Fatalf("Workspace relocation transfer protocol lacks %q", required)
		}
	}
	for _, forbidden := range []string{"host_path", "local_path", "storage_object"} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("Workspace relocation transfer protocol contains forbidden %q", forbidden)
		}
	}
}

func TestLocalWorkspaceProtocolCannotCarryPathsOrImageBytes(t *testing.T) {
	schema := string(readRepoFile(t, "contracts/runner/v1/runner.proto"))
	start := strings.Index(schema, "message LocalWorkspaceCommand {")
	end := strings.Index(schema, "// Evidence is deliberately")
	if start == -1 || end == -1 || end <= start {
		t.Fatal("local Workspace protocol section is missing")
	}
	section := schema[start:end]
	for _, forbidden := range []string{
		"bytes data",
		"host_path",
		"local_path",
		"storage_object",
		"sha256",
		"checkpoint",
	} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("local Workspace protocol contains forbidden %q", forbidden)
		}
	}
	for _, required := range []string{
		"string sandbox_id",
		"string workspace_id",
		"string snapshot_id",
		"string effect_id",
		"uint64 expected_generation",
		"uint64 logical_capacity_bytes",
		"bytes fencing_token",
	} {
		if !strings.Contains(section, required) {
			t.Fatalf("local Workspace protocol lacks %q", required)
		}
	}
}

func TestFrozenDescriptors(t *testing.T) {
	descriptors := []struct {
		proto      string
		descriptor string
		digest     string
	}{
		{
			proto:      "contracts/runner/v1/runner.proto",
			descriptor: "contracts/runner/v1/runner.descriptor.pb",
			digest:     "contracts/runner/v1/runner.descriptor.sha256",
		},
		{
			proto:      "contracts/guest/v1/guest.proto",
			descriptor: "contracts/guest/v1/guest.descriptor.pb",
			digest:     "contracts/guest/v1/guest.descriptor.sha256",
		},
	}

	for _, descriptor := range descriptors {
		t.Run(filepath.Base(descriptor.proto), func(t *testing.T) {
			committed := readRepoFile(t, descriptor.descriptor)
			digestText := strings.Fields(string(readRepoFile(t, descriptor.digest)))
			if len(digestText) != 2 {
				t.Fatalf("invalid digest file %s", descriptor.digest)
			}
			sum := sha256.Sum256(committed)
			if got := hex.EncodeToString(sum[:]); got != digestText[0] {
				t.Fatalf("descriptor digest = %s, want %s", got, digestText[0])
			}

			protoc, err := exec.LookPath("protoc")
			if err != nil {
				t.Fatal("protoc is required to prove committed descriptors match canonical schemas")
			}
			output := filepath.Join(t.TempDir(), filepath.Base(descriptor.descriptor))
			command := exec.Command(
				protoc,
				"-I", repositoryRoot(t),
				"--include_imports",
				"--descriptor_set_out="+output,
				filepath.Join(repositoryRoot(t), descriptor.proto),
			)
			generated, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("protoc failed: %v\n%s", err, generated)
			}
			if got := readFile(t, output); !bytes.Equal(got, committed) {
				t.Fatal("committed descriptor does not match its canonical schema")
			}
		})
	}
}

func negotiate(server, peer versionWindow, mandatory, available []string) (uint32, string) {
	if peer.Minimum == 0 || peer.Maximum == 0 || peer.Minimum > peer.Maximum {
		return 0, "invalid_range"
	}
	minimum := max(server.Minimum, peer.Minimum)
	maximum := min(server.Maximum, peer.Maximum)
	if minimum > maximum {
		return 0, "version_unsupported"
	}
	for _, feature := range mandatory {
		if !slices.Contains(available, feature) {
			return 0, "feature_unsupported"
		}
	}
	return maximum, ""
}

func classifyRunnerMessage(tc messageIdentityCase) string {
	if tc.IncomingFencingToken != tc.PreviousFencingToken {
		return "fenced"
	}
	if tc.IncomingSequence == tc.PreviousSequence {
		if tc.IncomingMessageID == tc.PreviousMessageID {
			return "duplicate"
		}
		return "identity_conflict"
	}
	if tc.IncomingSequence < tc.PreviousSequence {
		return "stale"
	}
	if tc.IncomingSequence > tc.PreviousSequence+1 {
		return "gap"
	}
	return "accept"
}

func classifyGuestFrame(tc frameIdentityCase, connectionNonce string, sandboxGeneration uint64) string {
	if tc.ConnectionNonce != connectionNonce || tc.SandboxGeneration != sandboxGeneration {
		return "binding_mismatch"
	}
	if tc.IncomingSequence == tc.PreviousSequence {
		return "duplicate"
	}
	if tc.IncomingSequence < tc.PreviousSequence {
		return "stale"
	}
	if tc.IncomingSequence > tc.PreviousSequence+1 {
		return "gap"
	}
	return "accept"
}

func testSchema(t *testing.T, path string, required []string) {
	t.Helper()
	content := string(readRepoFile(t, path))
	for _, declaration := range required {
		if !strings.Contains(content, declaration) {
			t.Errorf("%s is missing %q", path, declaration)
		}
	}
	for _, forbidden := range []string{"application_api_key", "end_user_identity", "model_provider_credential"} {
		if strings.Contains(strings.ToLower(content), forbidden) {
			t.Errorf("%s exposes forbidden field %q", path, forbidden)
		}
	}
}

func readFixture[T any](t *testing.T, path string) T {
	t.Helper()
	content := readRepoFile(t, path)
	var fixture T
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	canonical, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(content, canonical) {
		t.Fatalf("%s is not canonical deterministic JSON", path)
	}
	return fixture
}

func readRepoFile(t *testing.T, path string) []byte {
	t.Helper()
	return readFile(t, filepath.Join(repositoryRoot(t), path))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return content
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve protocol test path")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	if err != nil {
		t.Fatal(fmt.Errorf("resolve repository root: %w", err))
	}
	return root
}
