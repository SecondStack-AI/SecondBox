package deployconfig

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// ApplyStandardResources uses the same engine as the CLI after Compose has
// reported the control plane ready.
func ApplyStandardResources(ctx context.Context, resolved ResolvedDeployment, httpClient *http.Client) (resourceapply.Report, error) {
	if httpClient == nil {
		return resourceapply.Report{}, fmt.Errorf("SecondBox deployment resource HTTP client is required")
	}
	client, err := secondboxclient.NewSecondBoxClient(resolved.Manifest.Deployment.PublicBaseURL, resolved.Environment["SECONDBOX_PLATFORM_TOKEN"], httpClient)
	if err != nil {
		return resourceapply.Report{}, err
	}
	deadline := time.Duration(*resolved.Manifest.StandardResources.ApplyWaitSeconds) * time.Second
	waitContext, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		request, requestErr := http.NewRequestWithContext(waitContext, http.MethodGet, resolved.Manifest.Deployment.PublicBaseURL+"/readyz", nil)
		if requestErr != nil {
			return resourceapply.Report{}, requestErr
		}
		response, requestErr := httpClient.Do(request)
		if requestErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
			_ = response.Body.Close()
			break
		}
		if response != nil {
			_ = response.Body.Close()
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-waitContext.Done():
			timer.Stop()
			return resourceapply.Report{}, fmt.Errorf("SecondBox control plane did not become ready for resource apply: %w", waitContext.Err())
		case <-timer.C:
		}
	}
	return resourceapply.Apply(ctx, client, resolved.ResourceDocument)
}
