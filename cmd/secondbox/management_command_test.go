package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestManagementCommandsUseTypedAuthoritiesAndGeneratedRoutes(t *testing.T) {
	tenantRequest := writeManagementRequestFixture(t, `{"ref":"tenant-a","allowedProfileGrants":["durable-coding"],"allowedApplicationScopes":["sandbox:read"],"aggregateQuota":{"maxSandboxes":1,"maxActiveInstances":1,"maxCpuMillis":1000,"maxMemoryBytes":1073741824,"maxSnapshots":1,"maxPortSessions":1,"maxConcurrentOperations":1,"maxActiveSubjects":1,"maxApplicationAuthorities":1},"expiryPolicy":{"maximumSubjectLifetimeSeconds":3600,"maximumAuthorityLifetimeSeconds":3600},"metadata":{}}`)
	controllerRequest := writeManagementRequestFixture(t, `{"expiresAt":"2026-08-26T00:00:00Z","metadata":{}}`)
	subjectRequest := writeManagementRequestFixture(t, `{"ref":"subject-a","quota":{"maxSandboxes":1,"maxActiveInstances":1,"maxCpuMillis":1000,"maxMemoryBytes":1073741824,"maxSnapshots":1,"maxPortSessions":1,"maxConcurrentOperations":1},"metadata":{}}`)
	applicationRequest := writeManagementRequestFixture(t, `{"subjectRef":"subject-a","scopes":["sandbox:read"],"profileGrants":["durable-coding"],"expiresAt":"2026-08-26T00:00:00Z","metadata":{}}`)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "POST /v1/tenants":
			requireManagementRequestHeaders(t, request, "platform-token", "tenant-create")
			_, _ = io.WriteString(writer, `{"ref":"tenant-a","state":"active","revision":1,"allowedProfileGrants":["durable-coding"],"allowedApplicationScopes":["sandbox:read"],"aggregateQuota":{},"expiryPolicy":{},"metadata":{},"createdAt":"2026-08-25T00:00:00Z","updatedAt":"2026-08-25T00:00:00Z"}`)
		case "POST /v1/tenants/tenant-a/controller-authorities":
			requireManagementRequestHeaders(t, request, "platform-token", "controller-create")
			_, _ = io.WriteString(writer, `{"authority":{"id":"tca_1","tenantRef":"tenant-a","kind":"tenant_controller","grant":"tenant_management","lookupId":"lookup-1","state":"active","revision":1,"metadata":{},"createdAt":"2026-08-25T00:00:00Z","updatedAt":"2026-08-25T00:00:00Z"},"bearerToken":"controller-secret-once"}`)
		case "POST /v1/subjects":
			requireManagementRequestHeaders(t, request, "controller-token", "subject-create")
			if request.Header.Get("X-SecondBox-Tenant-Ref") != "" || request.Header.Get("X-SecondBox-Subject-Ref") != "" {
				t.Errorf("controller route received caller ownership assertions")
			}
			_, _ = io.WriteString(writer, `{"ref":"subject-a","tenantRef":"tenant-a","state":"active","cleanupState":"none","revision":1,"quota":{},"metadata":{},"createdAt":"2026-08-25T00:00:00Z","updatedAt":"2026-08-25T00:00:00Z"}`)
		case "POST /v1/application-authorities":
			requireManagementRequestHeaders(t, request, "controller-token", "application-create")
			_, _ = io.WriteString(writer, `{"authority":{"id":"app_1","tenantRef":"tenant-a","subjectRef":"subject-a","kind":"application","lookupId":"lookup-2","state":"active","revision":1,"scopes":["sandbox:read"],"profileGrants":["durable-coding"],"metadata":{},"createdAt":"2026-08-25T00:00:00Z","updatedAt":"2026-08-25T00:00:00Z"},"bearerToken":"application-secret-once"}`)
		default:
			t.Errorf("unexpected management request %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(writer, `{}`)
		}
	}))
	defer server.Close()

	platform := cliSession{url: server.URL, token: "platform-token", authority: sessionAuthorityPlatform}
	controller := cliSession{url: server.URL, token: "controller-token", authority: sessionAuthorityTenantController}
	commands := []struct {
		name       string
		session    cliSession
		args       []string
		wantSecret string
	}{
		{name: "tenant create", session: platform, args: []string{"tenant", "create", "--file", tenantRequest, "--idempotency-key", "tenant-create"}},
		{name: "controller create", session: platform, args: []string{"tenant", "controller-authority", "create", "tenant-a", "--file", controllerRequest, "--idempotency-key", "controller-create"}, wantSecret: "controller-secret-once"},
		{name: "subject create", session: controller, args: []string{"subject", "create", "--file", subjectRequest, "--idempotency-key", "subject-create"}},
		{name: "application create", session: controller, args: []string{"application-authority", "create", "--file", applicationRequest, "--idempotency-key", "application-create"}, wantSecret: "application-secret-once"},
	}
	for _, test := range commands {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			ctx := managementPresentationContext(&output, cliui.OutputPlain)
			handled, err := runManagementCommand(ctx, test.session, test.args, &output, server.Client())
			if err != nil || !handled {
				t.Fatalf("handled, error = %t, %v", handled, err)
			}
			if test.wantSecret != "" && strings.Count(output.String(), test.wantSecret) != 1 {
				t.Fatalf("one-time token count = %d in %q", strings.Count(output.String(), test.wantSecret), output.String())
			}
		})
	}
}

func TestManagementCredentialStructuredOutputCarriesBearerTokenOnce(t *testing.T) {
	requestPath := writeManagementRequestFixture(t, `{"expiresAt":"2026-08-26T00:00:00Z","metadata":{}}`)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"authority":{"id":"tca_1","tenantRef":"tenant-a","kind":"tenant_controller","grant":"tenant_management","lookupId":"lookup-1","state":"active","revision":1,"metadata":{},"createdAt":"2026-08-25T00:00:00Z","updatedAt":"2026-08-25T00:00:00Z"},"bearerToken":"structured-secret-once"}`)
	}))
	defer server.Close()
	var output bytes.Buffer
	ctx := managementPresentationContext(&output, cliui.OutputJSON)
	handled, err := runManagementCommand(ctx, cliSession{url: server.URL, token: "platform-token", authority: sessionAuthorityPlatform}, []string{"controller-authority", "create", "tenant-a", "--file", requestPath, "--idempotency-key", "create-key"}, &output, server.Client())
	if err != nil || !handled {
		t.Fatalf("handled, error = %t, %v", handled, err)
	}
	if strings.Count(output.String(), "structured-secret-once") != 1 {
		t.Fatalf("structured bearer token count = %d in %q", strings.Count(output.String(), "structured-secret-once"), output.String())
	}
	var response map[string]any
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response["bearerToken"] != "structured-secret-once" {
		t.Fatalf("structured credential response = %#v, %v", response, err)
	}
}

func TestManagementCommandActionsUseGeneratedRoutes(t *testing.T) {
	platform := sessionAuthorityPlatform
	controller := sessionAuthorityTenantController
	tests := []struct {
		name      string
		authority sessionAuthorityKind
		args      []string
		method    string
		path      string
		query     string
		mutation  bool
	}{
		{name: "tenant get", authority: platform, args: []string{"tenant", "get", "tenant-a"}, method: http.MethodGet, path: "/v1/tenants/tenant-a"},
		{name: "tenant list", authority: platform, args: []string{"tenant", "list", "--limit", "2", "--cursor", "next"}, method: http.MethodGet, path: "/v1/tenants", query: "cursor=next&limit=2"},
		{name: "tenant suspend", authority: platform, args: []string{"tenant", "suspend", "tenant-a", "--revision", "1", "--idempotency-key", "mutation"}, method: http.MethodPost, path: "/v1/tenants/tenant-a:suspend", mutation: true},
		{name: "tenant reactivate", authority: platform, args: []string{"tenant", "reactivate", "tenant-a", "--revision", "1", "--idempotency-key", "mutation"}, method: http.MethodPost, path: "/v1/tenants/tenant-a:reactivate", mutation: true},
		{name: "controller get", authority: platform, args: []string{"controller-authority", "get", "tenant-a", "controller-a"}, method: http.MethodGet, path: "/v1/tenants/tenant-a/controller-authorities/controller-a"},
		{name: "controller list", authority: platform, args: []string{"controller-authority", "list", "tenant-a", "--limit", "2"}, method: http.MethodGet, path: "/v1/tenants/tenant-a/controller-authorities", query: "limit=2"},
		{name: "controller rotate", authority: platform, args: []string{"controller-authority", "rotate", "tenant-a", "controller-a", "--revision", "1", "--idempotency-key", "mutation"}, method: http.MethodPost, path: "/v1/tenants/tenant-a/controller-authorities/controller-a:rotate", mutation: true},
		{name: "controller revoke", authority: platform, args: []string{"controller-authority", "revoke", "tenant-a", "controller-a", "--revision", "1", "--idempotency-key", "mutation"}, method: http.MethodPost, path: "/v1/tenants/tenant-a/controller-authorities/controller-a:revoke", mutation: true},
		{name: "subject get", authority: controller, args: []string{"subject", "get", "subject-a"}, method: http.MethodGet, path: "/v1/subjects/subject-a"},
		{name: "subject list", authority: controller, args: []string{"subject", "list", "--limit", "2"}, method: http.MethodGet, path: "/v1/subjects", query: "limit=2"},
		{name: "subject close", authority: controller, args: []string{"subject", "close", "subject-a", "--revision", "1", "--idempotency-key", "mutation"}, method: http.MethodPost, path: "/v1/subjects/subject-a:close", mutation: true},
		{name: "subject cleanup", authority: controller, args: []string{"subject", "cleanup", "subject-a", "--revision", "1", "--idempotency-key", "mutation"}, method: http.MethodPost, path: "/v1/subjects/subject-a:cleanup", mutation: true},
		{name: "application get", authority: controller, args: []string{"application-authority", "get", "application-a"}, method: http.MethodGet, path: "/v1/application-authorities/application-a"},
		{name: "application list", authority: controller, args: []string{"application-authority", "list", "--subject-ref", "subject-a", "--limit", "2"}, method: http.MethodGet, path: "/v1/application-authorities", query: "limit=2&subjectRef=subject-a"},
		{name: "application rotate", authority: controller, args: []string{"application-authority", "rotate", "application-a", "--revision", "1", "--idempotency-key", "mutation"}, method: http.MethodPost, path: "/v1/application-authorities/application-a:rotate", mutation: true},
		{name: "application revoke", authority: controller, args: []string{"application-authority", "revoke", "application-a", "--revision", "1", "--idempotency-key", "mutation"}, method: http.MethodPost, path: "/v1/application-authorities/application-a:revoke", mutation: true},
		{name: "tenant usage", authority: controller, args: []string{"tenant", "usage"}, method: http.MethodGet, path: "/v1/usage"},
		{name: "deployment usage", authority: platform, args: []string{"deployment", "usage", "--limit", "2"}, method: http.MethodGet, path: "/v1/deployment-usage", query: "limit=2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != test.method || request.URL.Path != test.path || request.URL.RawQuery != test.query {
					t.Errorf("management request = %s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery)
				}
				if request.Header.Get("Authorization") != "Bearer test-token" {
					t.Errorf("management authorization = %q", request.Header.Get("Authorization"))
				}
				if test.mutation && (request.Header.Get("Idempotency-Key") != "mutation" || request.Header.Get("If-Match") == "") {
					t.Errorf("management mutation headers = %#v", request.Header)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, `{}`)
			}))
			defer server.Close()
			var output bytes.Buffer
			ctx := managementPresentationContext(&output, cliui.OutputJSON)
			handled, err := runManagementCommand(ctx, cliSession{url: server.URL, token: "test-token", authority: test.authority}, test.args, &output, server.Client())
			if err != nil || !handled {
				t.Fatalf("handled, error = %t, %v", handled, err)
			}
		})
	}
}

func TestManagementCommandGroupsPropagateTypedAPIErrors(t *testing.T) {
	tests := []struct {
		name      string
		authority sessionAuthorityKind
		args      []string
	}{
		{name: "tenant", authority: sessionAuthorityPlatform, args: []string{"tenant", "get", "tenant-a"}},
		{name: "controller authority", authority: sessionAuthorityPlatform, args: []string{"controller-authority", "get", "tenant-a", "controller-a"}},
		{name: "subject", authority: sessionAuthorityTenantController, args: []string{"subject", "get", "subject-a"}},
		{name: "application authority", authority: sessionAuthorityTenantController, args: []string{"application-authority", "get", "application-a"}},
		{name: "usage", authority: sessionAuthorityTenantController, args: []string{"usage"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/problem+json")
				writer.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(writer, `{"code":"management_test_failure","status":409,"title":"management request failed"}`)
			}))
			defer server.Close()
			var output bytes.Buffer
			ctx := managementPresentationContext(&output, cliui.OutputJSON)
			handled, err := runManagementCommand(ctx, cliSession{url: server.URL, token: "test-token", authority: test.authority}, test.args, &output, server.Client())
			if !handled {
				t.Fatal("management command was not handled")
			}
			if code := secondboxclient.ProblemCodeOf(err); code != "management_test_failure" {
				t.Fatalf("management API problem code = %q, error = %v", code, err)
			}
			if output.Len() != 0 {
				t.Fatalf("failed management command rendered success output %q", output.String())
			}
		})
	}
}

func TestManagementCommandRejectsWrongStoredAuthorityKind(t *testing.T) {
	err := runTenantCommand(context.Background(), cliSession{url: "https://secondbox.example", token: "controller-token", authority: sessionAuthorityTenantController}, []string{"list"}, io.Discard, http.DefaultClient)
	var mismatch *cliSessionAuthorityError
	if !errors.As(err, &mismatch) || mismatch.Required != sessionAuthorityPlatform || mismatch.Actual != sessionAuthorityTenantController {
		t.Fatalf("authority mismatch = %#v, %v", mismatch, err)
	}
}

func writeManagementRequestFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireManagementRequestHeaders(t *testing.T, request *http.Request, token, idempotencyKey string) {
	t.Helper()
	if request.Header.Get("Authorization") != "Bearer "+token || request.Header.Get("Idempotency-Key") != idempotencyKey {
		t.Errorf("management headers = %#v", request.Header)
	}
}

func managementPresentationContext(output io.Writer, mode cliui.OutputMode) context.Context {
	capabilities := cliui.ForWriter(output, io.Discard)
	renderer := cliui.Renderer{Output: output, Diagnostic: io.Discard, Capabilities: capabilities, OutputMode: mode, ColorMode: cliui.ColorNever}
	return withPresentation(context.Background(), presentation{renderer: renderer})
}
