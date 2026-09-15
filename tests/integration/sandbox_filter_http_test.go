package integration_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestSandboxHTTPStateAndIDFiltersBindPagination(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "filter-http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "filter-http-profile")
	principal := authenticateCredential(t, controlPlane, credential)
	var sandboxes []contracts.Sandbox
	for i := 0; i < 3; i++ {
		sandbox, _, err := controlPlane.CreateSandbox(t.Context(), principal, fmt.Sprintf("filter-http-create-%d", i), contracts.CreateSandboxRequest{Image: testExecutionImage(), Profile: profile.Name, Metadata: map[string]string{"filter": "yes"}})
		if err != nil {
			t.Fatal(err)
		}
		sandboxes = append(sandboxes, sandbox)
	}
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	query := url.Values{"state": {sandboxes[0].State, "failed"}, "id": {sandboxes[0].ID, sandboxes[2].ID}, "metadata": {"filter=yes"}, "limit": {"1"}}
	read := func(q url.Values) contracts.SandboxPage {
		t.Helper()
		response := lifecycleHTTPRequest(t, server.URL, credential, http.MethodGet, "/v1/sandboxes?"+q.Encode(), "", "", "", nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("filter status=%d body=%s", response.StatusCode, readResponse(t, response))
		}
		var page contracts.SandboxPage
		decodeResponseJSON(t, response, &page)
		return page
	}
	first := read(query)
	if len(first.Items) != 1 || first.Items[0].ID != sandboxes[0].ID || first.NextCursor == nil {
		t.Fatalf("first filtered page=%+v", first)
	}
	query.Set("cursor", *first.NextCursor)
	query["id"] = []string{sandboxes[2].ID, sandboxes[0].ID}
	query["state"] = []string{"failed", sandboxes[0].State}
	second := read(query)
	if len(second.Items) != 1 || second.Items[0].ID != sandboxes[2].ID || second.NextCursor != nil {
		t.Fatalf("second filtered page=%+v", second)
	}
	query["id"] = []string{sandboxes[1].ID}
	response := lifecycleHTTPRequest(t, server.URL, credential, http.MethodGet, "/v1/sandboxes?"+query.Encode(), "", "", "", nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("changed filter status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	response.Body.Close()
	for _, suffix := range []string{"state=unknown", "id=bad"} {
		response := lifecycleHTTPRequest(t, server.URL, credential, http.MethodGet, "/v1/sandboxes?"+suffix, "", "", "", nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid filter status=%d body=%s", response.StatusCode, readResponse(t, response))
		}
		response.Body.Close()
	}
}
