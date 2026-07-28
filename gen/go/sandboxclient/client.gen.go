// Code generated from sandbox-service.openapi.json (sha256 78d7f7132f93eb4e415a11edb04184a05ac0b4952d8d97e582291c6d079ff679); DO NOT EDIT.

package sandboxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"secondstack/sandbox-service/pkg/contracts"
)

const ContractVersion = contracts.ContractVersionV1

type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

func New(rawURL, token string, httpClient *http.Client) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("Sandbox Service URL must be absolute")
	}
	if token == "" || httpClient == nil {
		return nil, errors.New("Sandbox Service token and HTTP client are required")
	}
	return &Client{baseURL: baseURL, token: token, httpClient: httpClient}, nil
}

func (c *Client) ResolveEnvironment(ctx context.Context, input contracts.ResolveEnvironmentRequest) (contracts.ResolveEnvironmentResponse, error) {
	var output contracts.ResolveEnvironmentResponse
	err := c.request(ctx, http.MethodPost, "/v1/environments:resolve", input, &output)
	return output, err
}

func (c *Client) GetEnvironment(ctx context.Context, id string) (contracts.LifecycleResponse, error) {
	var output contracts.LifecycleResponse
	err := c.request(ctx, http.MethodGet, "/v1/environments/"+url.PathEscape(id), nil, &output)
	return output, err
}

func (c *Client) GetWorkspaceUsage(ctx context.Context, tenantRef, subjectRef string) (contracts.WorkspaceUsage, error) {
	var output contracts.WorkspaceUsage
	endpoint := "/v1/workspace-usage?tenantRef=" + url.QueryEscape(tenantRef) + "&subjectRef=" + url.QueryEscape(subjectRef)
	err := c.request(ctx, http.MethodGet, endpoint, nil, &output)
	return output, err
}

func (c *Client) StartEnvironment(ctx context.Context, id string, input contracts.EnvironmentGenerationRequest) (contracts.LifecycleResponse, error) {
	var output contracts.LifecycleResponse
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":start", input, &output)
	return output, err
}

func (c *Client) InspectEnvironment(ctx context.Context, id string) (contracts.LifecycleResponse, error) {
	var output contracts.LifecycleResponse
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":inspect", nil, &output)
	return output, err
}

func (c *Client) StopEnvironment(ctx context.Context, id string, input contracts.EnvironmentGenerationRequest) (contracts.LifecycleResponse, error) {
	var output contracts.LifecycleResponse
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":stop", input, &output)
	return output, err
}

func (c *Client) ExecuteEnvironment(ctx context.Context, id string, input contracts.ExecuteRequest) (contracts.ExecuteResult, error) {
	var output contracts.ExecuteResult
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":execute", input, &output)
	return output, err
}

func (c *Client) PurgeEnvironment(ctx context.Context, id string, input contracts.PurgeEnvironmentRequest) error {
	return c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":purge", input, &struct{}{})
}

func (c *Client) CommitWorkspaceVersion(ctx context.Context, id string, input contracts.CommitWorkspaceVersionRequest) (contracts.WorkspaceVersion, error) {
	var output contracts.WorkspaceVersion
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+"/versions:commit", input, &output)
	return output, err
}

func (c *Client) GetCurrentWorkspaceVersion(ctx context.Context, id string) (contracts.WorkspaceVersion, error) {
	var output contracts.WorkspaceVersion
	err := c.request(ctx, http.MethodGet, "/v1/environments/"+url.PathEscape(id)+"/versions:current", nil, &output)
	return output, err
}

func (c *Client) GetWorkspaceVersion(ctx context.Context, id string, logicalVersion int64) (contracts.WorkspaceVersion, error) {
	var output contracts.WorkspaceVersion
	err := c.request(ctx, http.MethodGet, fmt.Sprintf("/v1/environments/%s/versions/%d", url.PathEscape(id), logicalVersion), nil, &output)
	return output, err
}

func (c *Client) MaterializeWorkspaceVersion(ctx context.Context, targetID string, input contracts.MaterializeWorkspaceVersionRequest) error {
	return c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(targetID)+":materialize", input, &struct{}{})
}

func (c *Client) CheckpointEnvironment(ctx context.Context, id string, input contracts.CheckpointRequest) (contracts.Snapshot, error) {
	var output contracts.Snapshot
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":checkpoint", input, &output)
	return output, err
}

func (c *Client) ExchangeArtifact(ctx context.Context, id string, input contracts.ExchangeArtifactRequest) (contracts.Artifact, error) {
	var output contracts.Artifact
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+"/artifacts:exchange", input, &output)
	return output, err
}

type WorkspaceFileWriteResult struct {
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

func (c *Client) OpenWorkspaceFile(ctx context.Context, environmentID string, expectedGeneration int64, leaseID, path string) (io.ReadCloser, int64, error) {
	endpoint := c.workspaceFileEndpoint(environmentID, expectedGeneration, leaseID, path)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failureErr := decodeFailure(response.Body)
		return nil, 0, errors.Join(failureErr, response.Body.Close())
	}
	return response.Body, response.ContentLength, nil
}

func (c *Client) PutWorkspaceFile(ctx context.Context, environmentID string, expectedGeneration int64, leaseID, path string, body io.Reader) (WorkspaceFileWriteResult, error) {
	var output WorkspaceFileWriteResult
	endpoint := c.workspaceFileEndpoint(environmentID, expectedGeneration, leaseID, path)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, body)
	if err != nil {
		return output, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return output, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failureErr := decodeFailure(response.Body)
		return output, errors.Join(failureErr, response.Body.Close())
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&output)
	return output, errors.Join(decodeErr, response.Body.Close())
}

func (c *Client) workspaceFileEndpoint(environmentID string, expectedGeneration int64, leaseID, path string) string {
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: "/v1/environments/" + url.PathEscape(environmentID) + "/files"})
	query := endpoint.Query()
	query.Set("expectedGeneration", fmt.Sprintf("%d", expectedGeneration))
	query.Set("leaseId", leaseID)
	query.Set("path", path)
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

func (c *Client) AcquireLease(ctx context.Context, id string, input contracts.AcquireLeaseRequest) (contracts.Lease, error) {
	var output contracts.Lease
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+"/leases:acquire", input, &output)
	return output, err
}

func (c *Client) RenewLease(ctx context.Context, id string, input contracts.RenewLeaseRequest) (contracts.Lease, error) {
	var output contracts.Lease
	err := c.request(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(id)+":renew", input, &output)
	return output, err
}

func (c *Client) ReleaseLease(ctx context.Context, id string) (contracts.Lease, error) {
	var output contracts.Lease
	err := c.request(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(id)+":release", nil, &output)
	return output, err
}

func (c *Client) ListResourceClasses(ctx context.Context) ([]contracts.ResourceClass, error) {
	var output []contracts.ResourceClass
	err := c.request(ctx, http.MethodGet, "/v1/resource-classes", nil, &output)
	return output, err
}

func (c *Client) ListLifecyclePolicies(ctx context.Context) ([]contracts.LifecyclePolicy, error) {
	var output []contracts.LifecyclePolicy
	err := c.request(ctx, http.MethodGet, "/v1/lifecycle-policies", nil, &output)
	return output, err
}

func (c *Client) request(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: path})
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failureErr := decodeFailure(response.Body)
		return errors.Join(failureErr, response.Body.Close())
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output)
	return errors.Join(decodeErr, response.Body.Close())
}

func decodeFailure(body io.Reader) error {
	var failure contracts.ErrorResponse
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&failure); err != nil {
		return err
	}
	return fmt.Errorf("Sandbox Service %s: %s", failure.Code, failure.Message)
}
