package scenarioharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

type Clients struct {
	Admin      *secondboxclient.Client
	Subject    *secondboxclient.Client
	HTTPClient *http.Client
}

func NewClients(
	baseURL string,
	platformToken string,
	applicationToken string,
	tenantRef string,
	subjectRef string,
	timeout time.Duration,
) (Clients, error) {
	httpClient := &http.Client{Timeout: timeout}
	admin, err := secondboxclient.NewSecondBoxClient(baseURL, platformToken, httpClient)
	if err != nil {
		return Clients{}, fmt.Errorf("SecondBox scenario administrative client: %w", err)
	}
	subject, err := secondboxclient.NewSecondBoxSubjectClient(
		baseURL,
		applicationToken,
		tenantRef,
		subjectRef,
		httpClient,
	)
	if err != nil {
		return Clients{}, fmt.Errorf("SecondBox scenario application client: %w", err)
	}
	return Clients{Admin: admin, Subject: subject, HTTPClient: httpClient}, nil
}

func RequestJSON[T any](
	ctx context.Context,
	client *secondboxclient.Client,
	operationID string,
	options secondboxclient.CallOptions,
) (T, error) {
	var result T
	if err := client.RequestJSON(ctx, operationID, options, &result); err != nil {
		return result, err
	}
	return result, nil
}

func JSONBody(value any) io.Reader {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("SecondBox scenario encode static request: %v", err))
	}
	return bytes.NewReader(data)
}

func IdempotencyHeaders(value string) http.Header {
	headers := make(http.Header)
	headers.Set("Idempotency-Key", value)
	return headers
}

func RevisionETag(revision int64) string {
	return `"revision-` + strconv.FormatInt(revision, 10) + `"`
}

func WaitSandbox(
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	states []secondboxclient.SandboxState,
	maximumWaitRequest time.Duration,
) (secondboxclient.Sandbox, error) {
	if len(states) == 0 {
		return secondboxclient.Sandbox{}, errors.New("SecondBox scenario wait requires terminal states")
	}
	if maximumWaitRequest <= 0 {
		return secondboxclient.Sandbox{}, errors.New("SecondBox scenario wait request bound must be positive")
	}
	target := make(map[secondboxclient.SandboxState]struct{}, len(states))
	for _, state := range states {
		target[state] = struct{}{}
	}
	last := handle.Snapshot()
	for {
		if _, found := target[last.State]; found {
			return last, nil
		}
		deadline, found := ctx.Deadline()
		if !found {
			return last, errors.New("SecondBox scenario Sandbox wait requires a context deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return last, fmt.Errorf(
				"SecondBox scenario Sandbox %s did not reach %v: last state=%s generation=%d: %w",
				last.ID,
				states,
				last.State,
				last.Generation,
				context.DeadlineExceeded,
			)
		}
		observed, err := handle.Wait(ctx, states, min(remaining, maximumWaitRequest))
		if err == nil {
			last = observed
			continue
		}
		var apiError *secondboxclient.APIError
		if errors.As(err, &apiError) &&
			apiError.Problem != nil &&
			apiError.Problem.Code == "wait_expired" {
			refreshed, refreshErr := handle.Refresh(ctx)
			if refreshErr != nil {
				return last, fmt.Errorf(
					"SecondBox scenario Sandbox %s refresh after wait expiry: %w",
					last.ID,
					refreshErr,
				)
			}
			last = refreshed
			continue
		}
		return last, fmt.Errorf(
			"SecondBox scenario Sandbox %s wait failed in state %s: %w",
			last.ID,
			last.State,
			err,
		)
	}
}
