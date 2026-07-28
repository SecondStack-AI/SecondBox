package agentservice

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExecutionRevokerCallsAuthenticatedEnvironmentStopHook(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != emergencyRevocationPath {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer agent-service-token" {
			t.Fatal("missing Agent Service credential")
		}
		var body struct {
			Reason   string   `json:"reason"`
			AgentIDs []string `json:"agentIds"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Reason != "environment_stopped" || len(body.AgentIDs) != 1 || body.AgentIDs[0] != "agent-1" {
			t.Fatalf("request = %+v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"cancelledWakes":1}`))
	}))
	defer server.Close()

	revoker, err := NewExecutionRevoker(server.URL, "agent-service-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := revoker.RevokeEnvironmentExecutions(t.Context(), "agent-1"); err != nil {
		t.Fatal(err)
	}
}
