package secondboxclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// OperationMediaType is one accepted request representation.
type OperationMediaType struct {
	ContentType string
	Schema      string
}

// OperationMetadata is the compact hand-maintained route description used by the thin client.
type OperationMetadata struct {
	OperationID         string
	Method              string
	PathTemplate        string
	RequestBody         []OperationMediaType
	RequestBodyRequired bool
}

func operation(id, method, path, contentType string) OperationMetadata {
	metadata := OperationMetadata{OperationID: id, Method: method, PathTemplate: path}
	if contentType != "" {
		metadata.RequestBody = []OperationMediaType{{ContentType: contentType}}
		metadata.RequestBodyRequired = true
	}
	return metadata
}

var operations = map[string]OperationMetadata{
	"acquireSandboxLease":      operation("acquireSandboxLease", "POST", "/v1/sandboxes/{sandboxId}/leases", "application/json"),
	"cancelSandboxExecStream":  operation("cancelSandboxExecStream", "POST", "/v1/sandboxes/{sandboxId}/exec-streams/{execSessionId}:cancel", ""),
	"cancelSandboxTerminal":    operation("cancelSandboxTerminal", "DELETE", "/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}", ""),
	"closeSandboxPortSession":  operation("closeSandboxPortSession", "DELETE", "/v1/sandboxes/{sandboxId}/port-sessions/{portSessionId}", ""),
	"createProfile":            operation("createProfile", "POST", "/v1/profiles", "application/json"),
	"createRunnerPool":         operation("createRunnerPool", "POST", "/v1/runner-pools", "application/json"),
	"createSandbox":            operation("createSandbox", "POST", "/v1/sandboxes", "application/json"),
	"createSandboxDirectory":   operation("createSandboxDirectory", "POST", "/v1/sandboxes/{sandboxId}/directories", "application/json"),
	"createSandboxExecStream":  operation("createSandboxExecStream", "POST", "/v1/sandboxes/{sandboxId}/exec-streams", "application/json"),
	"createSandboxPortSession": operation("createSandboxPortSession", "POST", "/v1/sandboxes/{sandboxId}/port-sessions", "application/json"),
	"createSandboxSnapshot":    operation("createSandboxSnapshot", "POST", "/v1/sandboxes/{sandboxId}/snapshots", "application/json"),
	"createSandboxTerminal":    operation("createSandboxTerminal", "POST", "/v1/sandboxes/{sandboxId}/terminals", "application/json"),
	"deleteArtifact":           operation("deleteArtifact", "DELETE", "/v1/artifacts/{artifactId}", ""),
	"deleteSandbox":            operation("deleteSandbox", "DELETE", "/v1/sandboxes/{sandboxId}", ""),
	"deleteSnapshot":           operation("deleteSnapshot", "DELETE", "/v1/snapshots/{snapshotId}", ""),
	"disableProfile":           operation("disableProfile", "POST", "/v1/profiles/{profileName}:disable", ""),
	"downloadArtifactContent":  operation("downloadArtifactContent", "GET", "/v1/artifacts/{artifactId}/content", ""),
	"drainSandbox":             operation("drainSandbox", "POST", "/v1/sandboxes/{sandboxId}:drain", ""),
	"executeSandboxCommand":    operation("executeSandboxCommand", "POST", "/v1/sandboxes/{sandboxId}/exec", "application/json"),
	"getArtifact":              operation("getArtifact", "GET", "/v1/artifacts/{artifactId}", ""),
	"getOperation":             operation("getOperation", "GET", "/v1/operations/{operationId}", ""),
	"getProfile":               operation("getProfile", "GET", "/v1/profiles/{profileName}", ""),
	"getRunner":                operation("getRunner", "GET", "/v1/runners/{runnerId}", ""),
	"getRunnerPool":            operation("getRunnerPool", "GET", "/v1/runner-pools/{runnerPoolName}", ""),
	"getSandbox":               operation("getSandbox", "GET", "/v1/sandboxes/{sandboxId}", ""),
	"getSandboxLease":          operation("getSandboxLease", "GET", "/v1/leases/{leaseId}", ""),
	"getSandboxPortSession":    operation("getSandboxPortSession", "GET", "/v1/sandboxes/{sandboxId}/port-sessions/{portSessionId}", ""),
	"getSnapshot":              operation("getSnapshot", "GET", "/v1/snapshots/{snapshotId}", ""),
	"inspectSandbox":           operation("inspectSandbox", "POST", "/v1/sandboxes/{sandboxId}:inspect", ""),
	"listProfiles":             operation("listProfiles", "GET", "/v1/profiles", ""),
	"listRunnerPools":          operation("listRunnerPools", "GET", "/v1/runner-pools", ""),
	"listRunners":              operation("listRunners", "GET", "/v1/runners", ""),
	"listSandboxArtifacts":     operation("listSandboxArtifacts", "GET", "/v1/sandboxes/{sandboxId}/artifacts", ""),
	"listSandboxDirectory":     operation("listSandboxDirectory", "GET", "/v1/sandboxes/{sandboxId}/directories", ""),
	"listSandboxes":            operation("listSandboxes", "GET", "/v1/sandboxes", ""),
	"listSandboxSnapshots":     operation("listSandboxSnapshots", "GET", "/v1/sandboxes/{sandboxId}/snapshots", ""),
	"pingSandbox":              operation("pingSandbox", "POST", "/v1/sandboxes/{sandboxId}:ping", ""),
	"readSandboxFile":          operation("readSandboxFile", "GET", "/v1/sandboxes/{sandboxId}/files", ""),
	"reconnectSandboxTerminal": operation("reconnectSandboxTerminal", "GET", "/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}", ""),
	"releaseSandboxLease":      operation("releaseSandboxLease", "DELETE", "/v1/leases/{leaseId}", ""),
	"removeSandboxPath":        operation("removeSandboxPath", "DELETE", "/v1/sandboxes/{sandboxId}/directories", "application/json"),
	"restoreSandboxSnapshot":   operation("restoreSandboxSnapshot", "POST", "/v1/sandboxes/{sandboxId}:restore", "application/json"),
	"renewSandboxLease":        operation("renewSandboxLease", "POST", "/v1/leases/{leaseId}:renew", "application/json"),
	"reviseProfile":            operation("reviseProfile", "POST", "/v1/profiles/{profileName}:revise", "application/json"),
	"sandboxFileExists":        operation("sandboxFileExists", "GET", "/v1/sandboxes/{sandboxId}/files:exists", ""),
	"startSandbox":             operation("startSandbox", "POST", "/v1/sandboxes/{sandboxId}:start", ""),
	"statSandboxFile":          operation("statSandboxFile", "GET", "/v1/sandboxes/{sandboxId}/files:stat", ""),
	"stopSandbox":              operation("stopSandbox", "POST", "/v1/sandboxes/{sandboxId}:stop", ""),
	"touchSandbox":             operation("touchSandbox", "POST", "/v1/sandboxes/{sandboxId}:touch", ""),
	"updateRunnerPool":         operation("updateRunnerPool", "PATCH", "/v1/runner-pools/{runnerPoolName}", "application/json"),
	"uploadSandboxArtifact":    operation("uploadSandboxArtifact", "POST", "/v1/sandboxes/{sandboxId}/artifacts", "multipart/form-data"),
	"waitForSandbox":           operation("waitForSandbox", "POST", "/v1/sandboxes/{sandboxId}:wait", "application/json"),
	"writeSandboxFile":         operation("writeSandboxFile", "PUT", "/v1/sandboxes/{sandboxId}/files", "application/octet-stream"),
}

// GetSandboxOperation is retained for callers that use the lower-level Do method.
var GetSandboxOperation = operations["getSandbox"]

// LookupOperation resolves one supported hand-maintained operation.
func LookupOperation(operationID string) (OperationMetadata, bool) {
	metadata, ok := operations[operationID]
	return metadata, ok
}

// RequestOptions supplies wire values to Do.
type RequestOptions struct {
	PathParameters  map[string]string
	QueryParameters url.Values
	Headers         http.Header
	Body            io.Reader
	ContentType     string
}

// APIError is a non-successful response with its structured problem when available.
type APIError struct {
	StatusCode int
	Problem    *Problem
	Body       []byte
}

func (failure *APIError) Error() string {
	if failure.Problem != nil {
		return fmt.Sprintf(
			"SecondBox API request failed: status=%d code=%s title=%s",
			failure.StatusCode, failure.Problem.Code, failure.Problem.Title,
		)
	}
	return fmt.Sprintf("SecondBox API request failed: status=%d", failure.StatusCode)
}

// Client is the thin dependency-free HTTP transport.
type Client struct {
	baseURL    *url.URL
	token      string
	tenantRef  string
	subjectRef string
	httpClient *http.Client
}

// NewSecondBoxClient constructs a client for trusted administrative callers.
func NewSecondBoxClient(rawURL, token string, httpClient *http.Client) (*Client, error) {
	return NewSecondBoxSubjectClient(
		rawURL, token, "secondbox", "secondbox-admin", httpClient,
	)
}

// NewSecondBoxSubjectClient validates transport and caller ownership values.
func NewSecondBoxSubjectClient(
	rawURL, token, tenantRef, subjectRef string,
	httpClient *http.Client,
) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" ||
		baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("SecondBox client URL must be an absolute HTTP endpoint without query or fragment")
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, errors.New("SecondBox client URL scheme must be http or https")
	}
	if token == "" {
		return nil, errors.New("SecondBox client platform token is required")
	}
	if strings.TrimSpace(tenantRef) == "" || strings.TrimSpace(subjectRef) == "" {
		return nil, errors.New("SecondBox client tenant and subject references are required")
	}
	if httpClient == nil {
		return nil, errors.New("SecondBox client HTTP client is required")
	}
	return &Client{
		baseURL: baseURL, token: token,
		tenantRef: tenantRef, subjectRef: subjectRef,
		httpClient: httpClient,
	}, nil
}

// Do sends one route and leaves successful response decoding to the caller.
func (client *Client) Do(
	ctx context.Context,
	metadata OperationMetadata,
	options RequestOptions,
) (*http.Response, error) {
	path := metadata.PathTemplate
	for {
		start := strings.IndexByte(path, '{')
		if start < 0 {
			break
		}
		endOffset := strings.IndexByte(path[start:], '}')
		if endOffset < 0 {
			return nil, fmt.Errorf("SecondBox client has malformed path template %q", path)
		}
		end := start + endOffset
		name := path[start+1 : end]
		value := options.PathParameters[name]
		if value == "" {
			return nil, fmt.Errorf(
				"SecondBox client missing required path parameter %q for %s",
				name, metadata.OperationID,
			)
		}
		path = path[:start] + url.PathEscape(value) + path[end+1:]
	}
	endpoint := client.baseURL.ResolveReference(&url.URL{Path: path})
	endpoint.RawQuery = options.QueryParameters.Encode()
	contentType := options.ContentType
	if options.Body != nil && contentType == "" && len(metadata.RequestBody) == 1 {
		contentType = metadata.RequestBody[0].ContentType
	}
	if options.Body != nil && metadata.RequestBodyRequired && contentType == "" {
		return nil, fmt.Errorf("SecondBox client content type is required for %s", metadata.OperationID)
	}
	if contentType != "" &&
		(len(metadata.RequestBody) != 1 || metadata.RequestBody[0].ContentType != contentType) {
		return nil, fmt.Errorf(
			"SecondBox client content type %q is not declared for %s",
			contentType, metadata.OperationID,
		)
	}
	request, err := http.NewRequestWithContext(
		ctx, metadata.Method, endpoint.String(), options.Body,
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox client create %s request: %w", metadata.OperationID, err)
	}
	request.Header = options.Headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("X-SecondBox-Tenant-Ref", client.tenantRef)
	request.Header.Set("X-SecondBox-Subject-Ref", client.subjectRef)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("SecondBox client send %s request: %w", metadata.OperationID, err)
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return response, nil
	}
	defer response.Body.Close()
	const maximumProblemBytes = 4 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumProblemBytes+1))
	if err != nil {
		return nil, fmt.Errorf("SecondBox client read %s error response: %w", metadata.OperationID, err)
	}
	if len(body) > maximumProblemBytes {
		return nil, fmt.Errorf(
			"SecondBox client %s error response exceeds %d bytes",
			metadata.OperationID, maximumProblemBytes,
		)
	}
	failure := &APIError{StatusCode: response.StatusCode, Body: body}
	var problem Problem
	if json.Unmarshal(body, &problem) == nil {
		failure.Problem = &problem
	}
	return nil, failure
}

// EncodeJSONBody encodes one request without buffering a second copy.
func EncodeJSONBody(value any) (io.Reader, error) {
	var body strings.Builder
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		return nil, fmt.Errorf("SecondBox client encode JSON body: %w", err)
	}
	return strings.NewReader(body.String()), nil
}
