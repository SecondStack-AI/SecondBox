// Package initialv1client freezes the minimum public API client shape used by
// the initial v1 release-candidate upgrade qualification.
package initialv1client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Client retains only initial-v1 wire knowledge and deliberately ignores additive response fields.
type Client struct {
	baseURL    string
	credential string
	httpClient *http.Client
}

// NewClient constructs the frozen initial-v1 compatibility client.
func NewClient(baseURL string, credential string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(credential) == "" || httpClient == nil {
		return nil, errors.New("SecondBox initial-v1 client URL, credential, and HTTP client are required")
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), credential: credential, httpClient: httpClient,
	}, nil
}

// Operation is the initial-v1 asynchronous mutation projection.
type Operation struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandboxId"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
}

// Sandbox is the initial-v1 durable Sandbox projection.
type Sandbox struct {
	ID                string            `json:"id"`
	Profile           string            `json:"profile"`
	ProfileRevisionID string            `json:"profileRevisionId"`
	State             string            `json:"state"`
	DesiredState      string            `json:"desiredState"`
	Generation        int64             `json:"generation"`
	Metadata          map[string]string `json:"metadata"`
	Workspace         Workspace         `json:"workspace"`
}

// Workspace is the initial-v1 durable workspace projection.
type Workspace struct {
	ID                    string `json:"id"`
	Generation            int64  `json:"generation"`
	CurrentCheckpointID   string `json:"currentCheckpointId,omitempty"`
	CurrentCheckpointHash string `json:"currentCheckpointHash,omitempty"`
	CurrentCheckpointSize int64  `json:"currentCheckpointSize,omitempty"`
}

// CreateSandbox sends the frozen initial-v1 create request shape.
func (client *Client) CreateSandbox(
	ctx context.Context,
	idempotencyKey string,
	profile string,
	metadata map[string]string,
) (Operation, error) {
	body, err := json.Marshal(map[string]any{"profile": profile, "metadata": metadata})
	if err != nil {
		return Operation{}, fmt.Errorf("SecondBox initial-v1 create request encoding failed: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.baseURL+"/v1/sandboxes",
		bytes.NewReader(body),
	)
	if err != nil {
		return Operation{}, fmt.Errorf("SecondBox initial-v1 create request construction failed: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.credential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	request.Header.Set("X-Request-ID", "initial-v1-create-"+idempotencyKey)
	var operation Operation
	if err := client.decodeJSON(request, http.StatusAccepted, &operation); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

// GetSandbox reads one Sandbox through the frozen initial-v1 response shape.
func (client *Client) GetSandbox(ctx context.Context, sandboxID string) (Sandbox, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		client.baseURL+"/v1/sandboxes/"+sandboxID,
		nil,
	)
	if err != nil {
		return Sandbox{}, fmt.Errorf("SecondBox initial-v1 get request construction failed: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.credential)
	request.Header.Set("X-Request-ID", "initial-v1-get-"+sandboxID)
	var sandbox Sandbox
	if err := client.decodeJSON(request, http.StatusOK, &sandbox); err != nil {
		return Sandbox{}, err
	}
	return sandbox, nil
}

func (client *Client) decodeJSON(request *http.Request, expectedStatus int, target any) error {
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("SecondBox initial-v1 request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		return fmt.Errorf(
			"SecondBox initial-v1 response status = %d, want %d",
			response.StatusCode,
			expectedStatus,
		)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("SecondBox initial-v1 response decoding failed: %w", err)
	}
	return nil
}
