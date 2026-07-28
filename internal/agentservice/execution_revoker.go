// Package agentservice implements Sandbox Service outbound calls to Agent Service.
package agentservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const emergencyRevocationPath = "/api/internal/v1/execution/emergency-revocations"

// ExecutionRevoker calls the authenticated Agent execution-cancellation boundary.
type ExecutionRevoker struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewExecutionRevoker constructs the Sandbox-owned outbound Agent Service adapter.
func NewExecutionRevoker(baseURL, token string, client *http.Client) (*ExecutionRevoker, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("Agent Service URL is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("Agent Service token is required")
	}
	if client == nil {
		return nil, fmt.Errorf("Agent Service HTTP client is required")
	}
	return &ExecutionRevoker{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  client,
	}, nil
}

// RevokeEnvironmentExecutions fences all admitted execution for one Agent.
func (revoker *ExecutionRevoker) RevokeEnvironmentExecutions(ctx context.Context, agentID string) error {
	payload, err := json.Marshal(struct {
		Reason   string   `json:"reason"`
		AgentIDs []string `json:"agentIds"`
	}{
		Reason:   "environment_stopped",
		AgentIDs: []string{agentID},
	})
	if err != nil {
		return fmt.Errorf("encode Environment stop execution revocation: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		revoker.baseURL+emergencyRevocationPath,
		bytes.NewReader(payload),
	)
	if err != nil {
		return fmt.Errorf("create Environment stop execution revocation request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+revoker.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := revoker.client.Do(request)
	if err != nil {
		return fmt.Errorf("submit Environment stop execution revocation: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	closeErr := response.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read Environment stop execution revocation response: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close Environment stop execution revocation response: %w", closeErr)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf(
			"Agent Service rejected Environment stop execution revocation with HTTP %d: %s",
			response.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}
	return nil
}
