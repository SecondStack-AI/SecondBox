package integration_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// TestInvalidRequestProblemsNameTheRefusedInput proves invalid_request problems
// name the offending JSON field, header, or query parameter through the real
// handler, and that each problem body satisfies the published contract.
func TestInvalidRequestProblemsNameTheRefusedInput(t *testing.T) {
	controlPlane, _ := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	profile, _, err := controlPlane.CreateProfileIdempotent(
		t.Context(), admin, "create-problem-details-profile",
		contracts.CreateProfileRequest{Name: "profile-problem-details", Spec: testProfileSpec(1)},
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)

	invalidSpec := testProfileSpec(1)
	invalidSpec.Architecture = "sparc"
	unalignedSpec := testProfileSpec(1)
	unalignedSpec.Resources.MemoryBytes = 1<<30 + 1
	encode := func(value any) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	var withoutIdleSeconds map[string]any
	if err := json.Unmarshal([]byte(encode(contracts.CreateProfileRequest{Name: "profile-without-idle", Spec: testProfileSpec(1)})), &withoutIdleSeconds); err != nil {
		t.Fatal(err)
	}
	delete(withoutIdleSeconds["spec"].(map[string]any)["lifecycle"].(map[string]any), "idleSeconds")
	var negativePortSessions map[string]any
	if err := json.Unmarshal([]byte(encode(contracts.CreateProfileRequest{Name: "profile-negative-port-sessions", Spec: testProfileSpec(1)})), &negativePortSessions); err != nil {
		t.Fatal(err)
	}
	negativePortSessions["spec"].(map[string]any)["ports"] = []any{
		map[string]any{"name": "web", "port": 8080, "protocol": "http", "maximumSessions": 1, "maximumSessionSeconds": 60},
		map[string]any{"name": "debug", "port": 9222, "protocol": "http", "maximumSessions": -1, "maximumSessionSeconds": 60},
	}
	withoutHeaders := func(names ...string) func(http.Header) {
		return func(headers http.Header) {
			for _, name := range names {
				headers.Del(name)
			}
		}
	}
	for _, test := range []struct {
		name    string
		method  string
		path    string
		body    string
		headers func(http.Header)
		title   string
		details []contracts.ProblemDetail
	}{
		{
			name: "invalid Profile spec", method: http.MethodPost, path: "/v1/profiles",
			body:    encode(contracts.CreateProfileRequest{Name: "profile-invalid-architecture", Spec: invalidSpec}),
			title:   "SecondBox spec.architecture must be amd64 or arm64",
			details: []contracts.ProblemDetail{{Field: "spec.architecture", Reason: "must be amd64 or arm64"}},
		},
		{
			name: "unaligned Profile resources", method: http.MethodPost, path: "/v1/profiles",
			body:    encode(contracts.CreateProfileRequest{Name: "profile-unaligned", Spec: unalignedSpec}),
			title:   "SecondBox spec.resources.memoryBytes must use whole MiB (multiples of 1048576 bytes)",
			details: []contracts.ProblemDetail{{Field: "spec.resources.memoryBytes", Reason: "must use whole MiB (multiples of 1048576 bytes)"}},
		},
		{
			name: "unknown JSON field", method: http.MethodPost, path: "/v1/profiles",
			body:    `{"name":"profile-unknown-field","spec":` + encode(testProfileSpec(1)) + `,"surprise":true}`,
			title:   `SecondBox body contains unknown field "surprise"`,
			details: []contracts.ProblemDetail{{Field: "body", Reason: `contains unknown field "surprise"`}},
		},
		{
			name: "JSON type mismatch", method: http.MethodPost, path: "/v1/profiles",
			body:    strings.Replace(encode(contracts.CreateProfileRequest{Name: "profile-type", Spec: testProfileSpec(1)}), `"vcpuCount":1`, `"vcpuCount":"one"`, 1),
			title:   "SecondBox spec.resources.vcpuCount must be a JSON integer in range",
			details: []contracts.ProblemDetail{{Field: "spec.resources.vcpuCount", Reason: "must be a JSON integer in range"}},
		},
		{
			name: "missing nested Profile lifecycle field", method: http.MethodPost, path: "/v1/profiles",
			body:    encode(withoutIdleSeconds),
			title:   "SecondBox spec.lifecycle.idleSeconds is required",
			details: []contracts.ProblemDetail{{Field: "spec.lifecycle.idleSeconds", Reason: "is required"}},
		},
		{
			name: "negative Port session limit", method: http.MethodPost, path: "/v1/profiles",
			body:    encode(negativePortSessions),
			title:   "SecondBox spec.ports[1].maximumSessions must be null or an exact JSON integer from 0 to 9007199254740991",
			details: []contracts.ProblemDetail{{Field: "spec.ports[1].maximumSessions", Reason: "must be null or an exact JSON integer from 0 to 9007199254740991"}},
		},
		{
			name: "Profile resource ceiling value type", method: http.MethodPost, path: "/v1/profiles",
			body:    `{"name":"profile-ceiling-type","spec":{"resourceCeiling":{"memoryBytes":"large"}}}`,
			title:   "SecondBox spec.resourceCeiling.memoryBytes must be a JSON integer in range",
			details: []contracts.ProblemDetail{{Field: "spec.resourceCeiling.memoryBytes", Reason: "must be a JSON integer in range"}},
		},
		{
			name: "invalid image reference", method: http.MethodPost, path: "/v1/images:prepare",
			body:    `{"image":{"reference":"invalid"}}`,
			title:   "SecondBox image.reference must be a fully qualified tag or sha256 digest reference of at most 512 bytes",
			details: []contracts.ProblemDetail{{Field: "image.reference", Reason: "must be a fully qualified tag or sha256 digest reference of at most 512 bytes"}},
		},
		{
			name: "missing Idempotency-Key", method: http.MethodPost, path: "/v1/profiles",
			body:    encode(contracts.CreateProfileRequest{Name: "profile-without-key", Spec: testProfileSpec(1)}),
			headers: withoutHeaders("Idempotency-Key"),
			title:   "SecondBox Idempotency-Key must contain 8 to 200 characters from A-Z a-z 0-9 . _ ~ : + / = -",
			details: []contracts.ProblemDetail{{Field: "Idempotency-Key", Reason: "must contain 8 to 200 characters from A-Z a-z 0-9 . _ ~ : + / = -"}},
		},
		{
			name: "missing If-Match", method: http.MethodPost, path: "/v1/profiles/" + profile.Name + ":revise",
			body:    encode(contracts.ReviseProfileRequest{Spec: testProfileSpec(2)}),
			headers: withoutHeaders("If-Match"),
			title:   `SecondBox If-Match must contain a positive revision ETag such as "revision-1"`,
			details: []contracts.ProblemDetail{{Field: "If-Match", Reason: `must contain a positive revision ETag such as "revision-1"`}},
		},
		{
			name: "missing ownership headers", method: http.MethodPost, path: "/v1/profiles",
			body:    encode(contracts.CreateProfileRequest{Name: "profile-without-owner", Spec: testProfileSpec(1)}),
			headers: withoutHeaders("X-SecondBox-Tenant-Ref", "X-SecondBox-Subject-Ref"),
			title:   "Request is invalid",
			details: []contracts.ProblemDetail{
				{Field: "X-SecondBox-Tenant-Ref", Reason: "must contain 1 to 128 visible ASCII characters"},
				{Field: "X-SecondBox-Subject-Ref", Reason: "must contain 1 to 128 visible ASCII characters"},
			},
		},
		{
			name: "list limit", method: http.MethodGet, path: "/v1/profiles?limit=0",
			title:   "SecondBox limit must be an integer between 1 and 200",
			details: []contracts.ProblemDetail{{Field: "limit", Reason: "must be an integer between 1 and 200"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var body io.Reader
			if test.body != "" {
				body = strings.NewReader(test.body)
			}
			request, err := http.NewRequestWithContext(t.Context(), test.method, server.URL+test.path, body)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+testPlatformToken)
			request.Header.Set("X-SecondBox-Tenant-Ref", admin.TenantRef)
			request.Header.Set("X-SecondBox-Subject-Ref", admin.SubjectRef)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "problem-details-"+strings.ReplaceAll(test.name, " ", "-"))
			request.Header.Set("If-Match", `"revision-1"`)
			if test.headers != nil {
				test.headers(request.Header)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.StatusCode, readResponse(t, response))
			}
			var problem contracts.Problem
			decodeResponseJSON(t, response, &problem)
			if problem.Code != "invalid_request" || problem.Title != test.title || !reflect.DeepEqual(problem.Details, test.details) {
				t.Fatalf("problem=%+v", problem)
			}
		})
	}
	unchanged, err := controlPlane.GetProfile(t.Context(), admin, profile.Name)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != contracts.ProfileStateEnabled || unchanged.Revision != profile.Revision {
		t.Fatalf("refused requests changed the Profile: %+v", unchanged)
	}
}
