package secondboxclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestHandWrittenOperationCoverage(t *testing.T) {
	const expectedOperationCount = 53
	operationIDs := []string{
		"createSandbox",
		"startSandbox",
		"stopSandbox",
		"restoreSandboxSnapshot",
		"statSandboxFile",
		"executeSandboxCommand",
		"createSandboxTerminal",
	}
	for _, operationID := range operationIDs {
		if _, found := LookupOperation(operationID); !found {
			t.Errorf("LookupOperation(%q) did not find canonical operation", operationID)
		}
	}

	found := 0
	for _, operationID := range allSupportedOperationIDsForTest() {
		if _, exists := LookupOperation(operationID); exists {
			found++
		}
	}
	if found != expectedOperationCount {
		t.Fatalf("hand-written operation count = %d, want %d", found, expectedOperationCount)
	}
}

func TestSecondBoxClientSendsHandWrittenOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("request method = %s, want GET", request.Method)
		}
		if request.URL.Path != "/v1/sandboxes/sandbox-1" {
			t.Errorf("request path = %s, want /v1/sandboxes/sandbox-1", request.URL.Path)
		}
		if request.URL.Query().Get("include") != "instance" {
			t.Errorf("include query = %q, want instance", request.URL.Query().Get("include"))
		}
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization header = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"id":"sandbox-1"}`)
	}))
	defer server.Close()

	client, err := NewSecondBoxClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(t.Context(), GetSandboxOperation, RequestOptions{
		PathParameters:  map[string]string{"sandboxId": "sandbox-1"},
		QueryParameters: url.Values{"include": []string{"instance"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("response status = %d, want 200", response.StatusCode)
	}
}

func allSupportedOperationIDsForTest() []string {
	return []string{
		"acquireSandboxLease",
		"cancelSandboxExecStream",
		"cancelSandboxTerminal",
		"closeSandboxPortSession",
		"createProfile",
		"createRunnerPool",
		"createSandbox",
		"createSandboxDirectory",
		"createSandboxExecStream",
		"createSandboxPortSession",
		"createSandboxSnapshot",
		"createSandboxTerminal",
		"deleteArtifact",
		"deleteSandbox",
		"deleteSnapshot",
		"disableProfile",
		"downloadArtifactContent",
		"executeSandboxCommand",
		"getArtifact",
		"getOperation",
		"getProfile",
		"getRunner",
		"getRunnerPool",
		"getSandbox",
		"getSandboxLease",
		"getSandboxPortSession",
		"getSnapshot",
		"inspectSandbox",
		"listProfiles",
		"listRunnerPools",
		"listRunners",
		"listSandboxArtifacts",
		"listSandboxDirectory",
		"listSandboxes",
		"listSandboxSnapshots",
		"pingSandbox",
		"readSandboxFile",
		"reconnectSandboxTerminal",
		"releaseSandboxLease",
		"removeSandboxPath",
		"renewSandboxLease",
		"restoreSandboxSnapshot",
		"reviseProfile",
		"sandboxFileExists",
		"startSandbox",
		"statSandboxFile",
		"stopSandbox",
		"touchSandbox",
		"updateRunnerPool",
		"uploadSandboxArtifact",
		"waitForSandbox",
		"writeSandboxFile",
		"drainSandbox",
	}
}
