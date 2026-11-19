package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type HTTPControlPlane struct {
	BaseURL string
	Token   string
	Pool    string
	Client  *http.Client
	Gateway GatewayIssuer
}

func (control HTTPControlPlane) request(ctx context.Context, method, path string, headers map[string]string, result any) error {
	base, err := url.Parse(control.BaseURL)
	if err != nil || (base.Scheme != "https" && !(base.Scheme == "http" && (base.Hostname() == "127.0.0.1" || base.Hostname() == "localhost"))) || control.Token == "" || control.Pool == "" || control.Client == nil {
		return errors.New("SecondBox fleet control-plane URL, authority, pool and HTTP client are required")
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(control.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+control.Token)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := control.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
		return errors.Join(fmt.Errorf("SecondBox fleet control %s failed: HTTP %d: %s", path, response.StatusCode, string(body)), readErr)
	}
	if result == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(result)
}

func (control HTTPControlPlane) Observe(ctx context.Context) (contracts.FleetPoolObservation, error) {
	var result contracts.FleetPoolObservation
	err := control.request(ctx, http.MethodGet, "/v1/runner-pools/"+url.PathEscape(control.Pool)+"/fleet", nil, &result)
	return result, err
}

func (control HTTPControlPlane) Evacuate(ctx context.Context, runner contracts.FleetRunner) error {
	return control.action(ctx, runner, "evacuate")
}
func (control HTTPControlPlane) Retire(ctx context.Context, runner contracts.FleetRunner) error {
	if control.Gateway == nil {
		return errors.New("SecondBox fleet gateway retirement authority is required")
	}
	if err := control.Gateway.Retire(ctx, runner.ID); err != nil {
		return err
	}
	return control.action(ctx, runner, "retire")
}
func (control HTTPControlPlane) action(ctx context.Context, runner contracts.FleetRunner, action string) error {
	return control.request(ctx, http.MethodPost, "/v1/runner-pools/"+url.PathEscape(control.Pool)+"/runners/"+url.PathEscape(runner.ID)+":"+action, map[string]string{"If-Match": fmt.Sprintf("\"%d\"", runner.Revision)}, nil)
}

func (control HTTPControlPlane) Archive(ctx context.Context, workspace contracts.FleetWorkspace) error {
	return control.request(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(workspace.SandboxID)+":archive", map[string]string{
		"X-SecondBox-Tenant-Ref": workspace.TenantRef, "X-SecondBox-Subject-Ref": workspace.SubjectRef,
		"If-Match": fmt.Sprintf("\"%d\"", workspace.Revision), "Idempotency-Key": fmt.Sprintf("fleet-archive-%s-%d", workspace.SandboxID, workspace.Revision),
	}, nil)
}
