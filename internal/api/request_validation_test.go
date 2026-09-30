package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/pagination"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func writeProblem(t *testing.T, err error) contracts.Problem {
	t.Helper()
	var logs bytes.Buffer
	apiHandler := &handler{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	writer := httptest.NewRecorder()
	writer.Header().Set("X-Request-ID", "request-invalid-field")
	apiHandler.writeError(writer, httptest.NewRequest(http.MethodPost, "/v1/profiles", nil), err)
	if writer.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", writer.Code, writer.Body.String())
	}
	var problem contracts.Problem
	if err := json.Unmarshal(writer.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "invalid_request" || logs.Len() != 0 {
		t.Fatalf("problem=%+v logs=%q", problem, logs.String())
	}
	return problem
}

func TestInvalidRequestProblemListsEveryJoinedFieldInOrder(t *testing.T) {
	err := fmt.Errorf("SecondBox request refused: %w", errors.Join(
		&ports.InvalidFieldError{Field: "X-SecondBox-Tenant-Ref", Reason: "must contain 1 to 128 visible ASCII characters"},
		errors.New("SecondBox private diagnostic"),
		errors.Join(&ports.InvalidFieldError{Field: "X-SecondBox-Subject-Ref", Reason: "is required"}),
	))
	problem := writeProblem(t, err)
	want := []contracts.ProblemDetail{
		{Field: "X-SecondBox-Tenant-Ref", Reason: "must contain 1 to 128 visible ASCII characters"},
		{Field: "X-SecondBox-Subject-Ref", Reason: "is required"},
	}
	if !reflect.DeepEqual(problem.Details, want) || problem.Title != "Request is invalid" {
		t.Fatalf("problem=%+v", problem)
	}
}

func TestInvalidRequestProblemTitlesASingleField(t *testing.T) {
	problem := writeProblem(t, &ports.InvalidFieldError{Field: "If-Match", Reason: "is required"})
	if problem.Title != "SecondBox If-Match is required" ||
		!reflect.DeepEqual(problem.Details, []contracts.ProblemDetail{{Field: "If-Match", Reason: "is required"}}) {
		t.Fatalf("problem=%+v", problem)
	}
}

func TestInvalidRequestProblemCapsDetailsAtTheContractBound(t *testing.T) {
	var fields []error
	for index := range maximumProblemDetails + 8 {
		fields = append(fields, &ports.InvalidFieldError{Field: fmt.Sprintf("field%d", index), Reason: "is invalid"})
	}
	problem := writeProblem(t, errors.Join(fields...))
	if len(problem.Details) != maximumProblemDetails ||
		problem.Details[0].Field != "field0" || problem.Details[maximumProblemDetails-1].Field != "field31" {
		t.Fatalf("details=%+v", problem.Details)
	}
}

func TestInvalidRequestProblemKeepsResourceAlignmentOutput(t *testing.T) {
	problem := writeProblem(t, ports.ResourceAlignmentError("resources.memoryBytes"))
	if problem.Title != "SecondBox resources.memoryBytes must use whole MiB (multiples of 1048576 bytes)" ||
		!reflect.DeepEqual(problem.Details, []contracts.ProblemDetail{{
			Field: "resources.memoryBytes", Reason: "must use whole MiB (multiples of 1048576 bytes)",
		}}) {
		t.Fatalf("problem=%+v", problem)
	}
}

func TestInvalidRequestProblemWithoutFieldsHasNoDetails(t *testing.T) {
	problem := writeProblem(t, errors.Join(ports.ErrInvalidRequest, errors.New("SecondBox private diagnostic")))
	if problem.Title != "Request is invalid" || problem.Details != nil {
		t.Fatalf("problem=%+v", problem)
	}
}

func TestInvalidRequestProblemNamesTheListCursor(t *testing.T) {
	problem := writeProblem(t, fmt.Errorf("SecondBox list failed: %w", pagination.ErrInvalidListCursor))
	if len(problem.Details) != 1 || problem.Details[0].Field != "cursor" {
		t.Fatalf("problem=%+v", problem)
	}
}

func TestInvalidRequestProblemKeepsAnOversizedSingleFieldOutOfTheTitle(t *testing.T) {
	field := &ports.InvalidFieldError{Field: "spec.ports", Reason: strings.Repeat("r", maximumProblemTitleBytes)}
	problem := writeProblem(t, field)
	if problem.Title != "Request is invalid" || len(problem.Details) != 1 {
		t.Fatalf("problem=%+v", problem)
	}
}

func TestDecodeStrictJSONNamesTheRefusedInput(t *testing.T) {
	for _, test := range []struct {
		name string
		body io.Reader
		want ports.InvalidFieldError
	}{
		{"missing body", nil, ports.InvalidFieldError{Field: "body", Reason: "is required"}},
		{"empty body", strings.NewReader(""), ports.InvalidFieldError{Field: "body", Reason: "is required"}},
		{"syntax", strings.NewReader(`{"name":`), ports.InvalidFieldError{Field: "body", Reason: "must be one well-formed JSON object of at most 1 MiB"}},
		{"malformed token", strings.NewReader(`{"name" 1}`), ports.InvalidFieldError{Field: "body", Reason: "must be one well-formed JSON object of at most 1 MiB"}},
		{"trailing value", strings.NewReader(`{"name":"a"} {}`), ports.InvalidFieldError{Field: "body", Reason: "must contain exactly one JSON object"}},
		{"unknown field", strings.NewReader(`{"name":"a","surprise":true}`), ports.InvalidFieldError{Field: "body", Reason: `contains unknown field "surprise"`}},
		{"oversized unknown field", strings.NewReader(`{"` + strings.Repeat("u", maximumEchoedJSONFieldNameBytes+1) + `":true}`), ports.InvalidFieldError{Field: "body", Reason: "contains an unknown field"}},
		{"top-level type", strings.NewReader(`[]`), ports.InvalidFieldError{Field: "body", Reason: "must be a JSON object"}},
		{"nested type", strings.NewReader(`{"spec":{"resources":{"vcpuCount":"two"}}}`), ports.InvalidFieldError{Field: "spec.resources.vcpuCount", Reason: "must be a JSON integer in range"}},
		{"custom decoder type", strings.NewReader(`{"spec":{"lifecycle":{"initialState":"stopped","drainGraceSeconds":1,"idleSeconds":null,"maximumDurationSeconds":null,"leaseSeconds":"x"}}}`), ports.InvalidFieldError{Field: "spec.lifecycle.leaseSeconds", Reason: "must be a JSON integer in range"}},
		{"string type", strings.NewReader(`{"name":7}`), ports.InvalidFieldError{Field: "name", Reason: "must be a JSON string"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/profiles", test.body)
			if test.body == nil {
				request.Body = nil
			}
			var body contracts.CreateProfileRequest
			err := decodeStrictJSON(request, &body)
			if !errors.Is(err, ports.ErrInvalidRequest) {
				t.Fatalf("error=%v is not invalid_request", err)
			}
			var field *ports.InvalidFieldError
			if !errors.As(err, &field) || *field != test.want {
				t.Fatalf("field=%+v want %+v (error %v)", field, test.want, err)
			}
			problem := writeProblem(t, err)
			if len(problem.Details) != 1 || problem.Details[0] != (contracts.ProblemDetail{Field: test.want.Field, Reason: test.want.Reason}) {
				t.Fatalf("problem=%+v", problem)
			}
			if strings.Contains(problem.Title, "json:") || strings.Contains(problem.Title, "int64") {
				t.Fatalf("problem title discloses decoder detail: %q", problem.Title)
			}
		})
	}
}

// A contracts decoder knows only the field inside its own value, so the
// problem must place its refusal at the member path where that value sits.
func TestDecodeStrictJSONLocatesContractsDecoderRefusals(t *testing.T) {
	const quotaWithoutMaxSandboxes = `"maxActiveInstances":1,"maxMemoryBytes":1,"maxSnapshots":1,"maxPortSessions":1,"maxConcurrentOperations":1,"maxActiveSubjects":1,"maxApplicationAuthorities":1`
	for _, test := range []struct {
		name        string
		body        string
		destination func() any
		want        ports.InvalidFieldError
	}{
		{
			"image reference", `{"image":{"reference":"invalid"}}`,
			func() any { return new(contracts.PrepareImageRequest) },
			ports.InvalidFieldError{Field: "image.reference", Reason: "must be a fully qualified tag or sha256 digest reference of at most 512 bytes"},
		},
		{
			"member matched case-insensitively", `{"Image":{"reference":"invalid"}}`,
			func() any { return new(contracts.PrepareImageRequest) },
			ports.InvalidFieldError{Field: "Image.reference", Reason: "must be a fully qualified tag or sha256 digest reference of at most 512 bytes"},
		},
		{
			"null start image", `{"image":null}`,
			func() any { return new(contracts.StartSandboxRequest) },
			ports.InvalidFieldError{Field: "image", Reason: "must be an object, not null"},
		},
		{
			"nested required policy field", `{"ref":"tenant-a","aggregateQuota":{` + quotaWithoutMaxSandboxes + `,"maxVcpuCount":1}}`,
			func() any { return new(contracts.CreateTenantRequest) },
			ports.InvalidFieldError{Field: "aggregateQuota.maxSandboxes", Reason: "is required"},
		},
		{
			"nested policy limit", `{"ref":"tenant-a","aggregateQuota":{"maxSandboxes":1,` + quotaWithoutMaxSandboxes + `,"maxVcpuCount":-1}}`,
			func() any { return new(contracts.CreateTenantRequest) },
			ports.InvalidFieldError{Field: "aggregateQuota.maxVcpuCount", Reason: "must be null or an exact JSON integer from 0 to 9007199254740991"},
		},
		{
			"sibling failure does not redirect", `{"ref":"tenant-a","aggregateQuota":{` + quotaWithoutMaxSandboxes + `,"maxVcpuCount":-1}}`,
			func() any { return new(contracts.CreateTenantRequest) },
			ports.InvalidFieldError{Field: "aggregateQuota.maxSandboxes", Reason: "is required"},
		},
		{
			"null Profile resource ceiling", `{"name":"profile-a","spec":{"resourceCeiling":null}}`,
			func() any { return new(contracts.CreateProfileRequest) },
			ports.InvalidFieldError{Field: "spec.resourceCeiling", Reason: "must be an object, not null"},
		},
		{
			"type mismatch behind a custom decoder boundary", `{"image":{"reference":123}}`,
			func() any { return new(contracts.StartSandboxRequest) },
			ports.InvalidFieldError{Field: "image.reference", Reason: "must be a JSON string"},
		},
		{
			"refusal inside an array element", `{"name":"profile-a","spec":{"ports":[{"port":8080,"maximumSessions":1},{"port":8081,"maximumSessions":-1}]}}`,
			func() any { return new(contracts.CreateProfileRequest) },
			ports.InvalidFieldError{Field: "spec.ports[1].maximumSessions", Reason: "must be null or an exact JSON integer from 0 to 9007199254740991"},
		},
		{
			"type mismatch inside an array element", `{"name":"profile-a","spec":{"ports":[{"port":8080},{"port":"http"}]}}`,
			func() any { return new(contracts.CreateProfileRequest) },
			ports.InvalidFieldError{Field: "spec.ports[1].port", Reason: "must be a JSON integer in range"},
		},
		{
			"type mismatch in a custom-decoded union member", `{"command":{"mode":"shell","command":123}}`,
			func() any { return new(contracts.BufferedExecRequest) },
			ports.InvalidFieldError{Field: "command.command", Reason: "must be a JSON string"},
		},
		{
			"type mismatch in a map value", `{"name":"profile-a","spec":{"resourceCeiling":{"memoryBytes":"large"}}}`,
			func() any { return new(contracts.CreateProfileRequest) },
			ports.InvalidFieldError{Field: "spec.resourceCeiling.memoryBytes", Reason: "must be a JSON integer in range"},
		},
		{
			"malformed timestamp", `{"subjectRef":"subject-a","expiresAt":"tomorrow"}`,
			func() any { return new(contracts.CreateApplicationAuthorityRequest) },
			ports.InvalidFieldError{Field: "expiresAt", Reason: "must be an RFC 3339 timestamp string"},
		},
		{
			"non-string timestamp", `{"subjectRef":"subject-a","expiresAt":1790000000}`,
			func() any { return new(contracts.CreateApplicationAuthorityRequest) },
			ports.InvalidFieldError{Field: "expiresAt", Reason: "must be an RFC 3339 timestamp string"},
		},
		{
			"timestamp behind a custom decoder boundary", `{"attributedExecution":{"authorizationRef":"authorization-a","expiresAt":"2026-13-01T00:00:00Z"}}`,
			func() any { return new(contracts.BufferedExecRequest) },
			ports.InvalidFieldError{Field: "attributedExecution.expiresAt", Reason: "must be an RFC 3339 timestamp string"},
		},
		{
			"null exec attribution", `{"attributedExecution":null}`,
			func() any { return new(contracts.StreamingExecRequest) },
			ports.InvalidFieldError{Field: "attributedExecution", Reason: "must be an object, not null"},
		},
		{
			"Exec command mode", `{"command":{"mode":"script"}}`,
			func() any { return new(contracts.BufferedExecRequest) },
			ports.InvalidFieldError{Field: "command.mode", Reason: "must be shell or argv"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/example", strings.NewReader(test.body))
			err := decodeStrictJSON(request, test.destination())
			var field *ports.InvalidFieldError
			if !errors.Is(err, ports.ErrInvalidRequest) || !errors.As(err, &field) || *field != test.want {
				t.Fatalf("field=%+v want %+v (error %v)", field, test.want, err)
			}
		})
	}
}

// Data-plane admission joins a named field to its classification sentinel;
// the problem must keep the sentinel's status and code and name the field.
func TestDataPlaneProfileBoundRefusalsNameTheirField(t *testing.T) {
	for _, sentinel := range []error{runnercontrol.ErrDataPlaneOutputLimit, runnercontrol.ErrDataPlaneStreamWindow} {
		field := &ports.InvalidFieldError{Field: "maximumOutputBytes", Reason: "must not exceed the pinned Profile's execution.maximumBufferedOutputBytes"}
		problem := writeProblem(t, errors.Join(field, sentinel))
		if problem.Title != field.Error() || !reflect.DeepEqual(problem.Details, []contracts.ProblemDetail{{Field: field.Field, Reason: field.Reason}}) {
			t.Fatalf("%v problem=%+v", sentinel, problem)
		}
	}
}

func TestBoundedJSONPathJoinsMembersAndIndices(t *testing.T) {
	for _, test := range []struct {
		path []string
		want string
	}{
		{nil, "body"},
		{[]string{"spec", "ports", "[1]", "maximumSessions"}, "spec.ports[1].maximumSessions"},
		{[]string{strings.Repeat("a", 128), strings.Repeat("b", 128)}, "body"},
	} {
		if got := boundedJSONPath(test.path); got != test.want {
			t.Errorf("boundedJSONPath(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestPlatformAuthenticationNamesEachInvalidOwnershipHeader(t *testing.T) {
	apiHandler := &handler{
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		platformTokenHash: sha256.Sum256([]byte("platform-token-for-invalid-headers")),
	}
	protected := apiHandler.authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("request with invalid ownership headers reached the handler")
	}))
	for _, test := range []struct {
		name    string
		tenant  string
		subject string
		fields  []string
	}{
		{"both missing", "", "", []string{"X-SecondBox-Tenant-Ref", "X-SecondBox-Subject-Ref"}},
		{"subject missing", "tenant-a", "", []string{"X-SecondBox-Subject-Ref"}},
		{"tenant too long", strings.Repeat("t", 129), "subject-a", []string{"X-SecondBox-Tenant-Ref"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/profiles/profile-a:disable", nil)
			request.Header.Set("Authorization", "Bearer platform-token-for-invalid-headers")
			request.Header.Set("X-SecondBox-Tenant-Ref", test.tenant)
			request.Header.Set("X-SecondBox-Subject-Ref", test.subject)
			writer := httptest.NewRecorder()
			protected.ServeHTTP(writer, request)
			var problem contracts.Problem
			if err := json.Unmarshal(writer.Body.Bytes(), &problem); err != nil {
				t.Fatal(err)
			}
			var fields []string
			for _, detail := range problem.Details {
				fields = append(fields, detail.Field)
			}
			if writer.Code != http.StatusBadRequest || problem.Code != "invalid_request" || !reflect.DeepEqual(fields, test.fields) {
				t.Fatalf("status=%d problem=%+v", writer.Code, problem)
			}
		})
	}
}
