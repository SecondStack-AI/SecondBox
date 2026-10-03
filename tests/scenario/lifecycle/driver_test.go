package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"syscall"
	"testing"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	scenarioharness "github.com/SecondStack-AI/SecondBox/tests/scenario/harness"
)

// A Sandbox the create Operation named but whose representation could not be
// fetched still exists on the deployment. Cell cleanup can only delete handles
// it was given, so createSandbox must hand one back even on that error path — a
// leaked Sandbox counts against subjectMaxSandboxes for as long as it lives, and
// enough of them manufacture a refusal that reads as saturation.
func TestSandboxHandleAddressedByIDIsEnoughToCleanUp(t *testing.T) {
	handle := secondboxclient.NewSandboxHandle(
		nil, secondboxclient.Sandbox{ID: "sandbox-01HZY"},
	)
	if handle == nil {
		t.Fatal("no handle was built from a known Sandbox identifier")
	}
	if handle.Snapshot().ID != "sandbox-01HZY" {
		t.Fatalf("handle identifier = %q", handle.Snapshot().ID)
	}
}

// cellResources must skip nil rather than record it, so a create that genuinely
// produced nothing cannot put a nil into the cleanup list.
func TestCellResourcesSkipsAbsentHandles(t *testing.T) {
	resources := &cellResources{}
	resources.add(nil)
	if len(resources.snapshot()) != 0 {
		t.Fatalf("cell resources recorded an absent handle: %d", len(resources.snapshot()))
	}
	resources.add(secondboxclient.NewSandboxHandle(
		nil, secondboxclient.Sandbox{ID: "sandbox-01HZZ"},
	))
	if len(resources.snapshot()) != 1 {
		t.Fatalf("cell resources = %d handles, want 1", len(resources.snapshot()))
	}
}

// Cleanup must survive the connection churn a saturated deployment produces.
// When the server times out a long poll it closes the connection, so the next
// request on a pooled keep-alive connection is reset -- precisely when a
// capacity run is under strain. Giving up there leaks the Sandbox, and leaked
// Sandboxes count against the subject quota for the rest of the ladder.
func TestTransientTransportErrorsAreDistinguishedFromAnswers(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		err       error
		transient bool
	}{
		{name: "connection reset", err: syscall.ECONNRESET, transient: true},
		{name: "broken pipe", err: syscall.EPIPE, transient: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, transient: true},
		{
			name:      "wrapped reset",
			err:       fmt.Errorf("send getSandbox request: %w", syscall.ECONNRESET),
			transient: true,
		},
		{name: "no error", err: nil, transient: false},
		{
			name:      "a plain answer is not transient",
			err:       errors.New("SecondBox lifecycle Sandbox reached failed instead of ready"),
			transient: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isTransientTransportError(testCase.err); got != testCase.transient {
				t.Fatalf("transient = %v, want %v for %v", got, testCase.transient, testCase.err)
			}
		})
	}
}

// preparedArchitectures runs prepare against a recording API and returns the
// RunnerPool architectures and Profile architecture it requested.
func preparedArchitectures(t *testing.T, prepare func(admin, subject *secondboxclient.Client) error) ([]string, string) {
	t.Helper()
	var pool secondboxclient.CreateRunnerPoolRequest
	var profile secondboxclient.CreateProfileRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/runner-pools":
			if err := json.NewDecoder(request.Body).Decode(&pool); err != nil {
				t.Error(err)
			}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(secondboxclient.RunnerPool{Name: pool.Name, State: pool.State, Architectures: pool.Architectures})
		case "/v1/profiles":
			if err := json.NewDecoder(request.Body).Decode(&profile); err != nil {
				t.Error(err)
			}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]any{"name": profile.Name, "currentRevision": map[string]any{"id": "prv_test"}})
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	clients, err := scenarioharness.NewClients(server.URL, "platform-token", "application-token", "tenant", "subject", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepare(clients.Admin, clients.Subject); err != nil {
		t.Fatal(err)
	}
	return pool.Architectures, profile.Spec.Architecture
}

func TestLifecyclePrepareBindsPoolAndProfileToTheBundleArchitecture(t *testing.T) {
	for _, architecture := range []string{"amd64", "arm64"} {
		pools, profile := preparedArchitectures(t, func(admin, subject *secondboxclient.Client) error {
			driver := &lifecycleDriver{config: validLifecycleConfig(), admin: admin, client: subject, architecture: architecture}
			return driver.prepare(t.Context())
		})
		if !slices.Equal(pools, []string{architecture}) || profile != architecture {
			t.Fatalf("%s lifecycle preparation requested pool %v and Profile %q", architecture, pools, profile)
		}
	}
}
