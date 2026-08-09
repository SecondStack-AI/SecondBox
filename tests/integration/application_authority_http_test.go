package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/api"
)

func TestApplicationAuthoritiesBindOwnershipProfilesScopesAndAdministrativeDenial(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	agentProject, agentAccount, _ := createProjectAccountAndCredential(
		t, controlPlane, admin, "application-agent",
	)
	runtimeProject, runtimeAccount, _ := createProjectAccountAndCredential(
		t, controlPlane, admin, "application-runtime",
	)
	agentProfile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, agentAccount, "application-agent-profile",
	)
	runtimeProfile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, runtimeAccount, "application-runtime-profile",
	)
	const agentToken = "secondbox-agent-application-token-000000000001"
	const runtimeToken = "secondbox-runtime-application-token-0000000001"
	handler, err := api.NewHandler(api.HandlerConfig{
		Service:       controlPlane,
		PlatformToken: testPlatformToken,
		ApplicationAuthorities: []api.ApplicationAuthority{
			{
				ID: "agent-service", Token: agentToken,
				TenantRef: agentProject.ID, SubjectRef: agentAccount.ID,
				Scopes:        []string{"sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files"},
				ProfileGrants: []string{agentProfile.Name},
			},
			{
				ID: "agent-runtime", Token: runtimeToken,
				TenantRef: runtimeProject.ID, SubjectRef: runtimeAccount.ID,
				Scopes:        []string{"sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files", "sandbox:ports"},
				ProfileGrants: []string{runtimeProfile.Name},
			},
		},
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)

	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/profiles/"+agentProfile.Name,
		agentToken, agentProject.ID, agentAccount.ID, "", nil,
	), http.StatusOK)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/profiles/"+runtimeProfile.Name,
		agentToken, agentProject.ID, agentAccount.ID, "", nil,
	), http.StatusForbidden)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/runners",
		agentToken, agentProject.ID, agentAccount.ID, "", nil,
	), http.StatusForbidden)
	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes",
		agentToken, runtimeProject.ID, runtimeAccount.ID, "", nil,
	), http.StatusForbidden)

	created := applicationRequest(
		t, http.MethodPost, server.URL+"/v1/sandboxes",
		agentToken, agentProject.ID, agentAccount.ID, "application-agent-create",
		map[string]any{"profile": agentProfile.Name, "metadata": map[string]string{"owner": "agent"}},
	)
	if created.StatusCode != http.StatusAccepted {
		t.Fatalf("granted Profile create status = %d body=%s", created.StatusCode, readResponse(t, created))
	}
	created.Body.Close()

	assertHTTPStatusAndClose(t, applicationRequest(
		t, http.MethodPost, server.URL+"/v1/sandboxes",
		agentToken, agentProject.ID, agentAccount.ID, "application-runtime-profile-denied",
		map[string]any{"profile": runtimeProfile.Name, "metadata": map[string]string{"owner": "agent"}},
	), http.StatusForbidden)

	runtimeList := applicationRequest(
		t, http.MethodGet, server.URL+"/v1/sandboxes",
		runtimeToken, runtimeProject.ID, runtimeAccount.ID, "", nil,
	)
	if runtimeList.StatusCode != http.StatusOK {
		t.Fatalf("runtime Sandbox list status = %d body=%s", runtimeList.StatusCode, readResponse(t, runtimeList))
	}
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(runtimeList.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	runtimeList.Body.Close()
	if len(page.Items) != 0 {
		t.Fatalf("runtime authority observed %d Agent Service Sandboxes", len(page.Items))
	}
}

func applicationRequest(
	t *testing.T,
	method string,
	url string,
	token string,
	tenantRef string,
	subjectRef string,
	idempotencyKey string,
	body any,
) *http.Response {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-SecondBox-Tenant-Ref", tenantRef)
	request.Header.Set("X-SecondBox-Subject-Ref", subjectRef)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
