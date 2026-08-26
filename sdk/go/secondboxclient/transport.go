package secondboxclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// OperationMediaType is one accepted request representation.
type OperationMediaType struct {
	ContentType string
	Schema      string
}

// OperationMetadata is the compact generated route description used by the thin client.
type OperationMetadata struct {
	OperationID         string
	Method              string
	PathTemplate        string
	RequestBody         []OperationMediaType
	RequestBodyRequired bool
}

// LookupOperation resolves one supported generated operation.
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
	if strings.TrimSpace(tenantRef) == "" || strings.TrimSpace(subjectRef) == "" {
		return nil, errors.New("SecondBox client tenant and subject references are required")
	}
	return newSecondBoxClient(rawURL, token, tenantRef, subjectRef, httpClient)
}

// NewSecondBoxTenantControllerClient constructs a tenant-controller client without caller-supplied ownership assertions.
func NewSecondBoxTenantControllerClient(rawURL, token string, httpClient *http.Client) (*Client, error) {
	return newSecondBoxClient(rawURL, token, "", "", httpClient)
}

func newSecondBoxClient(rawURL, token, tenantRef, subjectRef string, httpClient *http.Client) (*Client, error) {
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
	rawPath := metadata.PathTemplate
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
		rawStart := strings.IndexByte(rawPath, '{')
		rawEndOffset := strings.IndexByte(rawPath[rawStart:], '}')
		rawEnd := rawStart + rawEndOffset
		path = path[:start] + value + path[end+1:]
		rawPath = rawPath[:rawStart] + url.PathEscape(value) + rawPath[rawEnd+1:]
	}
	endpoint := client.baseURL.ResolveReference(&url.URL{Path: path, RawPath: rawPath})
	endpoint.RawQuery = options.QueryParameters.Encode()
	contentType := options.ContentType
	if options.Body != nil && contentType == "" && len(metadata.RequestBody) == 1 {
		contentType = metadata.RequestBody[0].ContentType
	}
	if options.Body != nil && metadata.RequestBodyRequired && contentType == "" {
		return nil, fmt.Errorf("SecondBox client content type is required for %s", metadata.OperationID)
	}
	if contentType != "" && !declaresContentType(metadata, contentType) {
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
	if client.tenantRef != "" {
		request.Header.Set("X-SecondBox-Tenant-Ref", client.tenantRef)
	}
	if client.subjectRef != "" {
		request.Header.Set("X-SecondBox-Subject-Ref", client.subjectRef)
	}
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

func declaresContentType(metadata OperationMetadata, contentType string) bool {
	if len(metadata.RequestBody) != 1 {
		return false
	}
	actual, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	declared, _, err := mime.ParseMediaType(metadata.RequestBody[0].ContentType)
	return err == nil && actual == declared
}

// EncodeJSONBody encodes one request without buffering a second copy.
func EncodeJSONBody(value any) (io.Reader, error) {
	var body strings.Builder
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		return nil, fmt.Errorf("SecondBox client encode JSON body: %w", err)
	}
	return strings.NewReader(body.String()), nil
}
