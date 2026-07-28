package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const maxSourceBindingResponseBytes = 1 << 20

type IntegrationSourceBindingClient struct {
	baseURL *url.URL
	token   string
	client  *http.Client
}

func NewIntegrationSourceBindingClient(rawURL, token string, client *http.Client) (*IntegrationSourceBindingClient, error) {
	baseURL, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.Path != "" {
		return nil, errors.New("Integration Service URL must be an absolute HTTP origin")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("Integration Service token is required")
	}
	if client == nil {
		return nil, errors.New("Integration Service HTTP client is required")
	}
	return &IntegrationSourceBindingClient{baseURL: baseURL, token: token, client: client}, nil
}

func (c *IntegrationSourceBindingClient) Register(ctx context.Context, binding SourceBinding) (SourceBindingRegistration, error) {
	var registration SourceBindingRegistration
	err := c.request(ctx, "/internal/v1/egress/source-bindings:register", integrationSourceBindingRegisterRequest{
		EnvironmentID:        binding.EnvironmentID,
		InstanceID:           binding.InstanceID,
		SourceAddress:        binding.SourceAddress,
		Generation:           strconv.FormatInt(binding.Generation, 10),
		AllowedConnectionIDs: binding.AllowedConnectionIDs,
	}, &registration)
	if err != nil {
		return SourceBindingRegistration{}, err
	}
	if strings.TrimSpace(registration.SourceToken) == "" {
		return SourceBindingRegistration{}, errors.New("Integration Service returned an empty source token")
	}
	return registration, nil
}

func (c *IntegrationSourceBindingClient) Unregister(ctx context.Context, binding SourceBinding) error {
	var response integrationSourceBindingMutationResponse
	return c.request(ctx, "/internal/v1/egress/source-bindings:unregister", integrationSourceBindingUnregisterRequest{
		EnvironmentID: binding.EnvironmentID,
		InstanceID:    binding.InstanceID,
		SourceAddress: binding.SourceAddress,
		Generation:    strconv.FormatInt(binding.Generation, 10),
	}, &response)
}

type integrationSourceBindingRegisterRequest struct {
	EnvironmentID        string   `json:"environmentId"`
	InstanceID           string   `json:"instanceId"`
	SourceAddress        string   `json:"sourceAddress"`
	Generation           string   `json:"generation"`
	AllowedConnectionIDs []string `json:"allowedConnectionIds"`
}

type integrationSourceBindingUnregisterRequest struct {
	EnvironmentID string `json:"environmentId"`
	InstanceID    string `json:"instanceId"`
	SourceAddress string `json:"sourceAddress"`
	Generation    string `json:"generation"`
}

type integrationSourceBindingMutationResponse struct {
	ContractVersion string `json:"contractVersion"`
	Deleted         bool   `json:"deleted"`
}

func (c *IntegrationSourceBindingClient) request(ctx context.Context, path string, input, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode Integration source binding: %w", err)
	}
	endpoint := *c.baseURL
	endpoint.Path = path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create Integration source-binding request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("Integration source-binding transport: %w", err)
	}
	responsePayload, readErr := io.ReadAll(io.LimitReader(response.Body, maxSourceBindingResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("read Integration source-binding response: %w", errors.Join(readErr, closeErr))
	}
	if len(responsePayload) > maxSourceBindingResponseBytes {
		return errors.New("Integration source-binding response exceeds limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Integration Service rejected source binding with HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responsePayload)))
	}
	if len(responsePayload) == 0 {
		responsePayload = []byte(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(responsePayload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode Integration source-binding response: %w", err)
	}
	return nil
}
