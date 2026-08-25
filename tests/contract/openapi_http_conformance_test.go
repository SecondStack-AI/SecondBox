package contract_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/service"
)

type auditedHTTPOperation struct {
	handler         string
	successStatus   string
	successBody     string
	requestHeaders  []string
	responseHeaders []string
}

var auditedV1HTTPOperations = map[string]auditedHTTPOperation{
	"listTenants":                     {"listTenants", "200", "TenantPage", nil, nil},
	"createTenant":                    {"createTenant", "201", "Tenant", []string{"Idempotency-Key"}, []string{"ETag", "Idempotency-Replayed"}},
	"getTenant":                       {"getTenant", "200", "Tenant", nil, []string{"ETag"}},
	"suspendTenant":                   {"tenantManagementAction", "200", "Tenant", []string{"Idempotency-Key", "If-Match"}, []string{"ETag", "Idempotency-Replayed"}},
	"reactivateTenant":                {"tenantManagementAction", "200", "Tenant", []string{"Idempotency-Key", "If-Match"}, []string{"ETag", "Idempotency-Replayed"}},
	"listTenantControllerAuthorities": {"listTenantControllerAuthorities", "200", "TenantControllerAuthorityPage", nil, nil},
	"createTenantControllerAuthority": {"createTenantControllerAuthority", "201", "TenantControllerCredentialResponse", []string{"Idempotency-Key"}, []string{"ETag", "Idempotency-Replayed"}},
	"getTenantControllerAuthority":    {"getTenantControllerAuthority", "200", "TenantControllerAuthority", nil, []string{"ETag"}},
	"rotateTenantControllerAuthority": {"tenantControllerAuthorityManagementAction", "200", "TenantControllerCredentialResponse", []string{"Idempotency-Key", "If-Match"}, []string{"ETag", "Idempotency-Replayed"}},
	"revokeTenantControllerAuthority": {"tenantControllerAuthorityManagementAction", "200", "TenantControllerAuthority", []string{"Idempotency-Key", "If-Match"}, []string{"ETag", "Idempotency-Replayed"}},
	"listSubjects":                    {"listSubjects", "200", "SubjectPage", nil, nil},
	"createSubject":                   {"createSubject", "201", "Subject", []string{"Idempotency-Key"}, []string{"ETag", "Idempotency-Replayed"}},
	"getSubject":                      {"getSubject", "200", "Subject", nil, []string{"ETag"}},
	"updateSubjectQuota":              {"updateSubjectQuota", "200", "Subject", []string{"Idempotency-Key", "If-Match"}, []string{"ETag", "Idempotency-Replayed"}},
	"closeSubject":                    {"subjectManagementAction", "200", "Subject", []string{"Idempotency-Key", "If-Match"}, []string{"ETag", "Idempotency-Replayed"}},
	"cleanupSubject":                  {"subjectManagementAction", "202", "Operation", []string{"Idempotency-Key", "If-Match"}, []string{"Idempotency-Replayed"}},
	"listApplicationAuthorities":      {"listApplicationAuthorities", "200", "ApplicationAuthorityPage", nil, nil},
	"createApplicationAuthority":      {"createApplicationAuthority", "201", "ApplicationCredentialResponse", []string{"Idempotency-Key"}, []string{"ETag", "Idempotency-Replayed"}},
	"getApplicationAuthority":         {"getApplicationAuthority", "200", "ApplicationAuthority", nil, []string{"ETag"}},
	"rotateApplicationAuthority":      {"applicationAuthorityManagementAction", "200", "ApplicationCredentialResponse", []string{"Idempotency-Key", "If-Match"}, []string{"ETag", "Idempotency-Replayed"}},
	"revokeApplicationAuthority":      {"applicationAuthorityManagementAction", "200", "ApplicationAuthority", []string{"Idempotency-Key", "If-Match"}, []string{"ETag", "Idempotency-Replayed"}},
	"getTenantUsage":                  {"getTenantUsage", "200", "TenantUsage", nil, nil},
	"getDeploymentUsage":              {"getDeploymentUsage", "200", "DeploymentUsage", nil, nil},
	"listProfiles":                    {"listProfiles", "200", "ProfilePage", nil, nil},
	"createProfile":                   {"createProfile", "201", "Profile", []string{"Idempotency-Key"}, []string{"ETag", "Idempotency-Replayed"}},
	"getProfile":                      {"getProfile", "200", "Profile", nil, []string{"ETag"}},
	"reviseProfile":                   {"mutateProfile", "200", "Profile", []string{"Idempotency-Key", "If-Match"}, []string{"ETag", "Idempotency-Replayed"}},
	"disableProfile":                  {"mutateProfile", "200", "Profile", []string{"Idempotency-Key", "If-Match"}, []string{"ETag", "Idempotency-Replayed"}},
	"listRunnerPools":                 {"listRunnerPools", "200", "RunnerPoolPage", nil, nil},
	"createRunnerPool":                {"createRunnerPool", "201", "RunnerPool", nil, []string{"ETag"}},
	"getRunnerPool":                   {"getRunnerPool", "200", "RunnerPool", nil, []string{"ETag"}},
	"updateRunnerPool":                {"updateRunnerPool", "200", "RunnerPool", []string{"If-Match"}, []string{"ETag"}},
	"listRunners":                     {"listRunners", "200", "RunnerPage", nil, nil},
	"getRunner":                       {"getRunner", "200", "Runner", nil, []string{"ETag"}},
	"getDeploymentTiming":             {"getDeploymentTiming", "200", "DeploymentTimingSummary", nil, nil},
	"listSandboxes":                   {"listSandboxes", "200", "SandboxPage", nil, nil},
	"createSandbox":                   {"createSandbox", "202", "Operation", []string{"Idempotency-Key"}, []string{"Idempotency-Replayed"}},
	"getSandbox":                      {"getSandbox", "200", "Sandbox", nil, []string{"ETag"}},
	"updateSandboxMetadata":           {"updateSandboxMetadata", "200", "Sandbox", []string{"If-Match"}, []string{"ETag"}},
	"getSandboxTiming":                {"getSandboxTiming", "200", "SandboxTiming", nil, nil},
	"deleteSandbox":                   {"mutateSandboxLifecycle", "202", "Operation", []string{"Idempotency-Key", "If-Match"}, []string{"Idempotency-Replayed"}},
	"startSandbox":                    {"mutateSandboxLifecycle", "202", "Operation", []string{"Idempotency-Key", "If-Match"}, []string{"Idempotency-Replayed"}},
	"inspectSandbox":                  {"mutateSandbox", "200", "SandboxInspection", []string{"SecondBox-Generation"}, nil},
	"pingSandbox":                     {"mutateSandbox", "200", "PingResult", []string{"SecondBox-Generation"}, nil},
	"touchSandbox":                    {"mutateSandbox", "200", "TouchResult", []string{"Idempotency-Key", "SecondBox-Generation"}, nil},
	"drainSandbox":                    {"mutateSandboxLifecycle", "202", "Operation", []string{"Idempotency-Key", "If-Match"}, []string{"Idempotency-Replayed"}},
	"stopSandbox":                     {"mutateSandboxLifecycle", "202", "Operation", []string{"Idempotency-Key", "If-Match"}, []string{"Idempotency-Replayed"}},
	"relocateSandbox":                 {"mutateSandbox", "202", "Operation", []string{"Idempotency-Key", "If-Match"}, []string{"Idempotency-Replayed"}},
	"restoreSandboxSnapshot":          {"mutateSandbox", "202", "Operation", []string{"Idempotency-Key", "If-Match"}, []string{"Idempotency-Replayed"}},
	"waitForSandbox":                  {"mutateSandbox", "200", "Sandbox", nil, []string{"ETag"}},
	"getOperation":                    {"getOperation", "200", "Operation", nil, nil},
	"getOperationTiming":              {"getOperationTiming", "200", "OperationTiming", nil, nil},
	"acquireSandboxLease":             {"acquireLease", "201", "Lease", []string{"Idempotency-Key", "SecondBox-Generation"}, nil},
	"getSandboxLease":                 {"getLease", "200", "Lease", nil, nil},
	"releaseSandboxLease":             {"releaseLease", "200", "Lease", []string{"Idempotency-Key"}, nil},
	"renewSandboxLease":               {"renewLease", "200", "Lease", []string{"Idempotency-Key"}, nil},
	"executeSandboxCommand":           {"executeSandboxCommand", "200", "ExecOutcome", []string{"Idempotency-Key", "SecondBox-Generation"}, []string{"Idempotency-Replayed"}},
	"createSandboxExecStream":         {"createSandboxExecStream", "201", "ExecStreamSession", []string{"Idempotency-Key", "SecondBox-Generation"}, []string{"Idempotency-Replayed"}},
	"cancelSandboxExecStream":         {"cancelSandboxExecStream", "202", "ExecStreamSession", []string{"Idempotency-Key", "SecondBox-Generation"}, []string{"Idempotency-Replayed"}},
	"createSandboxTerminal":           {"createSandboxTerminal", "201", "TerminalSession", []string{"Idempotency-Key", "SecondBox-Generation"}, []string{"Idempotency-Replayed"}},
	"reconnectSandboxTerminal":        {"getOrConnectSandboxTerminal", "200", "TerminalSession", []string{"SecondBox-Generation"}, nil},
	"cancelSandboxTerminal":           {"cancelSandboxTerminal", "202", "TerminalSession", []string{"Idempotency-Key", "SecondBox-Generation"}, []string{"Idempotency-Replayed"}},
	"readSandboxFile":                 {"readSandboxFile", "200", "string", []string{"SecondBox-Generation"}, []string{"Content-Length", "Digest"}},
	"writeSandboxFile":                {"writeSandboxFile", "200", "FileWriteResult", []string{"Digest", "Idempotency-Key", "SecondBox-Generation"}, []string{"Idempotency-Replayed"}},
	"statSandboxFile":                 {"statSandboxFile", "200", "FileStat", []string{"SecondBox-Generation"}, nil},
	"sandboxFileExists":               {"sandboxFileExists", "200", "FileExistsResult", []string{"SecondBox-Generation"}, nil},
	"listSandboxDirectory":            {"listSandboxDirectory", "200", "DirectoryListing", []string{"SecondBox-Generation"}, nil},
	"createSandboxDirectory":          {"createSandboxDirectory", "204", "", []string{"Idempotency-Key", "SecondBox-Generation"}, []string{"Idempotency-Replayed"}},
	"removeSandboxPath":               {"removeSandboxPath", "204", "", []string{"Idempotency-Key", "SecondBox-Generation"}, []string{"Idempotency-Replayed"}},
	"listSandboxSnapshots":            {"listSandboxSnapshots", "200", "SnapshotPage", nil, nil},
	"createSandboxSnapshot":           {"createSandboxSnapshot", "202", "Operation", []string{"Idempotency-Key", "If-Match"}, []string{"Idempotency-Replayed"}},
	"getSnapshot":                     {"getSnapshot", "200", "Snapshot", nil, nil},
	"deleteSnapshot":                  {"deleteSnapshot", "202", "Operation", []string{"Idempotency-Key"}, []string{"Idempotency-Replayed"}},
	"createSandboxPortSession":        {"createSandboxPortSession", "201", "PortSession", []string{"Idempotency-Key", "SecondBox-Generation"}, []string{"Idempotency-Replayed"}},
	"getSandboxPortSession":           {"getSandboxPortSession", "200", "PortSession", nil, nil},
	"closeSandboxPortSession":         {"closeSandboxPortSession", "204", "", []string{"Idempotency-Key"}, nil},
}

var failClosedManagementOperations = map[string]bool{
	"closeSubject": true, "cleanupSubject": true,
}

func TestCanonicalOpenAPIHTTPConformanceInventory(t *testing.T) {
	document := loadOpenAPIContract(t)
	seen := map[string]bool{}
	paths := object(t, document["paths"], "paths")
	for path, pathValue := range paths {
		pathItem := resolveOpenAPIObject(t, document, pathValue, path)
		for _, method := range []string{"delete", "get", "patch", "post", "put"} {
			operationValue, exists := pathItem[method]
			if !exists {
				continue
			}
			operation := resolveOpenAPIObject(t, document, operationValue, method+" "+path)
			operationID, _ := operation["operationId"].(string)
			audited, exists := auditedV1HTTPOperations[operationID]
			if !exists {
				t.Errorf("%s %s operation %q has no audited HTTP behavior", method, path, operationID)
				continue
			}
			seen[operationID] = true
			assertAuditedSuccessResponse(t, document, operationID, operation, audited)
			assertAuditedRequestHeaders(t, document, operationID, operation, audited.requestHeaders)
		}
	}
	for operationID := range auditedV1HTTPOperations {
		if !seen[operationID] {
			t.Errorf("audited HTTP operation %q is absent from canonical OpenAPI", operationID)
		}
	}
}

func TestCanonicalOpenAPIOperationsReachHTTPRouter(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpHandler, err := api.NewHandler(api.HandlerConfig{
		Service: &service.ControlPlaneService{}, Logger: logger,
		PlatformToken:             "contract-platform-token-at-least-24-bytes",
		MaximumDataPlaneBodyBytes: 1024,
	})
	if err != nil {
		t.Fatalf("construct SecondBox HTTP handler: %v", err)
	}

	document := loadOpenAPIContract(t)
	for path, pathValue := range object(t, document["paths"], "paths") {
		pathItem := resolveOpenAPIObject(t, document, pathValue, path)
		for _, method := range []string{"delete", "get", "patch", "post", "put"} {
			operationValue, exists := pathItem[method]
			if !exists {
				continue
			}
			operation := resolveOpenAPIObject(t, document, operationValue, method+" "+path)
			operationID := operation["operationId"].(string)
			t.Run(operationID, func(t *testing.T) {
				request := httptest.NewRequest(strings.ToUpper(method), concreteOpenAPIPath(path), nil)
				response := httptest.NewRecorder()
				httpHandler.ServeHTTP(response, request)
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("%s %s did not reach its authenticated route: status=%d body=%s", method, path, response.Code, response.Body.String())
				}
				if response.Header().Get("X-Request-ID") == "" {
					t.Fatalf("%s %s omitted X-Request-ID", method, path)
				}
			})
		}
	}
}

func TestManagementRoutesFailClosedAtTheirAuthorityBoundary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpHandler, err := api.NewHandler(api.HandlerConfig{
		Service: &service.ControlPlaneService{}, Logger: logger,
		PlatformToken:             "contract-platform-token-at-least-24-bytes",
		MaximumDataPlaneBodyBytes: 1024,
	})
	if err != nil {
		t.Fatalf("construct SecondBox HTTP handler: %v", err)
	}

	for _, test := range []struct {
		name  string
		path  string
		token string
	}{
		{name: "platform cannot escalate to tenant controller", path: "/v1/subjects", token: "contract-platform-token-at-least-24-bytes"},
		{name: "application cannot escalate to tenant controller", path: "/v1/subjects", token: ports.ApplicationBearerTokenPrefix + "contract-application-token-material"},
		{name: "application cannot escalate to platform", path: "/v1/tenants", token: ports.ApplicationBearerTokenPrefix + "contract-application-token-material"},
		{name: "tenant controller cannot inspect deployment usage", path: "/v1/deployment-usage", token: ports.TenantControllerBearerTokenPrefix + "contract-controller-token-material"},
		{name: "application cannot inspect deployment usage", path: "/v1/deployment-usage", token: ports.ApplicationBearerTokenPrefix + "contract-application-token-material"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Header.Set("Authorization", "Bearer "+test.token)
			response := httptest.NewRecorder()
			httpHandler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"authentication_failed"`) {
				t.Fatalf("authority boundary response status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	t.Run("undocumented management route is absent", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/v1/tenant-administration", nil)
		request.Header.Set("Authorization", "Bearer contract-platform-token-at-least-24-bytes")
		response := httptest.NewRecorder()
		httpHandler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("undocumented management route status=%d body=%s", response.Code, response.Body.String())
		}

		request = httptest.NewRequest(http.MethodPost, "/v1/tenants/tenant-one:escalate", nil)
		request.Header.Set("Authorization", "Bearer contract-platform-token-at-least-24-bytes")
		response = httptest.NewRecorder()
		httpHandler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("undocumented management action status=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestCanonicalOpenAPICoversEveryPublicHTTPRoute(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate SecondBox HTTP conformance test source")
	}
	httpSourcePath := filepath.Join(filepath.Dir(sourceFile), "..", "..", "internal", "api", "http.go")
	httpSource, err := os.ReadFile(httpSourcePath)
	if err != nil {
		t.Fatalf("read SecondBox HTTP router source: %v", err)
	}
	routePattern := regexp.MustCompile(`mux\.Handle(?:Func)?\("((?:GET|POST|PUT|PATCH|DELETE) /v1/[^"]+)"`)
	registeredRoutes := routePattern.FindAllStringSubmatch(string(httpSource), -1)
	if len(registeredRoutes) == 0 {
		t.Fatal("SecondBox HTTP router exposes no greppable canonical v1 registrations")
	}

	document := loadOpenAPIContract(t)
	canonicalRequests := canonicalOpenAPIRequests(t, document)
	allowedStreamingRoutes := map[string]bool{
		"GET /v1/sandboxes/{sandboxID}/exec-streams/{execSessionID}": true,
		"GET /v1/port-tunnels/{portSessionID}":                       true,
	}
	for _, match := range registeredRoutes {
		registered := match[1]
		if allowedStreamingRoutes[registered] {
			continue
		}
		mux := http.NewServeMux()
		mux.HandleFunc(registered, func(http.ResponseWriter, *http.Request) {})
		matched := false
		for _, request := range canonicalRequests {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request.Clone(request.Context()))
			if response.Code == http.StatusOK {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("public HTTP route %q has no canonical OpenAPI operation", registered)
		}
	}
}

func TestAuditedHTTPHandlersContainConformanceMarkers(t *testing.T) {
	handlerSources := loadHTTPHandlerFunctionSources(t)
	statusMarkers := map[string]string{
		"200": "http.StatusOK", "201": "http.StatusCreated",
		"202": "http.StatusAccepted", "204": "http.StatusNoContent",
	}
	requestHeaderMarkers := map[string]string{
		"Digest":               `Header.Get("Digest")`,
		"Idempotency-Key":      `Header.Get("Idempotency-Key")`,
		"If-Match":             "parseIfMatch(request)",
		"SecondBox-Generation": "parseGeneration(request)",
	}
	responseHeaderMarkers := map[string]string{
		"Digest":               `Header().Set("Digest"`,
		"ETag":                 "setRevisionETag(writer,",
		"Idempotency-Replayed": `Header().Set("Idempotency-Replayed"`,
	}
	for operationID, audited := range auditedV1HTTPOperations {
		source, exists := handlerSources[audited.handler]
		if !exists {
			t.Errorf("%s audited handler %q is absent", operationID, audited.handler)
			continue
		}
		if failClosedManagementOperations[operationID] {
			marker := ".managementUnavailable("
			if audited.handler == "managementUnavailable" {
				marker = "ErrManagementUnavailable"
			}
			if !strings.Contains(source, marker) {
				t.Errorf("%s fail-closed handler %s does not emit its typed management error", operationID, audited.handler)
			}
			continue
		}
		if marker := statusMarkers[audited.successStatus]; !strings.Contains(source, marker) {
			t.Errorf("%s handler %s does not contain audited success marker %s", operationID, audited.handler, marker)
		}
		for _, header := range audited.requestHeaders {
			if marker := requestHeaderMarkers[header]; !strings.Contains(source, marker) {
				t.Errorf("%s handler %s does not enforce required request header %s", operationID, audited.handler, header)
			}
		}
		for _, header := range audited.responseHeaders {
			if marker := responseHeaderMarkers[header]; !strings.Contains(source, marker) {
				t.Errorf("%s handler %s does not emit audited response header %s", operationID, audited.handler, header)
			}
		}
	}
}

func assertAuditedSuccessResponse(
	t *testing.T,
	document openAPIDocument,
	operationID string,
	operation map[string]any,
	audited auditedHTTPOperation,
) {
	t.Helper()
	responses := object(t, operation["responses"], operationID+".responses")
	successStatuses := []string{}
	for status := range responses {
		if strings.HasPrefix(status, "2") {
			successStatuses = append(successStatuses, status)
		}
	}
	sort.Strings(successStatuses)
	if strings.Join(successStatuses, ",") != audited.successStatus {
		t.Errorf("%s success statuses = %v, audited HTTP handler emits only %s", operationID, successStatuses, audited.successStatus)
		return
	}
	response := resolveOpenAPIObject(t, document, responses[audited.successStatus], operationID+" success response")
	actualBody := openAPIResponseBodyName(t, document, response, operationID)
	if actualBody != audited.successBody {
		t.Errorf("%s success body = %q, audited HTTP handler emits %q", operationID, actualBody, audited.successBody)
	}
	headers := object(t, response["headers"], operationID+" success headers")
	delete(headers, "X-Request-ID")
	actualHeaders := make([]string, 0, len(headers))
	for name := range headers {
		actualHeaders = append(actualHeaders, name)
	}
	sort.Strings(actualHeaders)
	expectedHeaders := append([]string(nil), audited.responseHeaders...)
	sort.Strings(expectedHeaders)
	if strings.Join(actualHeaders, ",") != strings.Join(expectedHeaders, ",") {
		t.Errorf("%s success headers = %v, audited HTTP handler emits %v plus X-Request-ID", operationID, actualHeaders, expectedHeaders)
	}
}

func assertAuditedRequestHeaders(
	t *testing.T,
	document openAPIDocument,
	operationID string,
	operation map[string]any,
	expected []string,
) {
	t.Helper()
	actual := []string{}
	for _, parameterValue := range arrayOrEmpty(operation["parameters"]) {
		parameter := resolveOpenAPIObject(t, document, parameterValue, operationID+" parameter")
		if parameter["in"] == "header" && parameter["required"] == true {
			actual = append(actual, parameter["name"].(string))
		}
	}
	sort.Strings(actual)
	expected = append([]string(nil), expected...)
	sort.Strings(expected)
	if strings.Join(actual, ",") != strings.Join(expected, ",") {
		t.Errorf("%s required request headers = %v, audited HTTP handler requires %v", operationID, actual, expected)
	}
}

func resolveOpenAPIObject(t *testing.T, document openAPIDocument, value any, context string) map[string]any {
	t.Helper()
	resolved := object(t, value, context)
	if reference, ok := resolved["$ref"].(string); ok {
		resolved = object(t, resolveLocalReference(t, document, reference), context+" reference")
	}
	return resolved
}

func openAPIResponseBodyName(
	t *testing.T,
	document openAPIDocument,
	response map[string]any,
	operationID string,
) string {
	t.Helper()
	contentValue, exists := response["content"]
	if !exists {
		return ""
	}
	content := object(t, contentValue, operationID+" success content")
	for _, mediaType := range []string{"application/json", "application/octet-stream"} {
		mediaValue, exists := content[mediaType]
		if !exists {
			continue
		}
		media := object(t, mediaValue, operationID+" "+mediaType)
		schema := object(t, media["schema"], operationID+" success schema")
		if reference, ok := schema["$ref"].(string); ok {
			return strings.TrimPrefix(reference, "#/components/schemas/")
		}
		if schemaType, ok := schema["type"].(string); ok {
			return schemaType
		}
	}
	return ""
}

func concreteOpenAPIPath(path string) string {
	pathParameter := regexp.MustCompile(`\{[^}]+\}`)
	return pathParameter.ReplaceAllString(path, "conformance-resource")
}

func canonicalOpenAPIRequests(t *testing.T, document openAPIDocument) []*http.Request {
	t.Helper()
	requests := []*http.Request{}
	for path, pathValue := range object(t, document["paths"], "paths") {
		pathItem := resolveOpenAPIObject(t, document, pathValue, path)
		for _, method := range []string{"delete", "get", "patch", "post", "put"} {
			if _, exists := pathItem[method]; exists {
				requests = append(requests, httptest.NewRequest(strings.ToUpper(method), concreteOpenAPIPath(path), nil))
			}
		}
	}
	return requests
}

func loadHTTPHandlerFunctionSources(t *testing.T) map[string]string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate SecondBox HTTP conformance test source")
	}
	apiDirectory := filepath.Join(filepath.Dir(sourceFile), "..", "..", "internal", "api")
	entries, err := os.ReadDir(apiDirectory)
	if err != nil {
		t.Fatalf("read SecondBox HTTP API directory: %v", err)
	}
	sources := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(apiDirectory, entry.Name())
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read SecondBox HTTP source %s: %v", entry.Name(), err)
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, contents, 0)
		if err != nil {
			t.Fatalf("parse SecondBox HTTP source %s: %v", entry.Name(), err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || function.Body == nil {
				continue
			}
			start := fileSet.Position(function.Pos()).Offset
			end := fileSet.Position(function.End()).Offset
			sources[function.Name.Name] = string(contents[start:end])
		}
	}
	return sources
}

func arrayOrEmpty(value any) []any {
	if value == nil {
		return nil
	}
	return value.([]any)
}
