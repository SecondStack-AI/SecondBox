package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestRunnerPoolHTTPRejectsRetiredCapacityPolicy(t *testing.T) {
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	controlPlane := newManagementControlPlane(t, databaseStore, time.Now().UTC())
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	t.Cleanup(server.Close)
	client, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := client.CreateRunnerPool(t.Context(), secondboxclient.CreateRunnerPoolRequest{Name: "pool-no-capacity", State: secondboxclient.RunnerPoolStateReady, Architectures: []string{"amd64"}, Capabilities: []string{"compute"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			path := "/v1/runner-pools"
			body := `{"name":"pool-retired-capacity","state":"ready","architectures":["amd64"],"capabilities":["compute"],"capacityPolicy":{"maxSandboxes":1}}`
			if method != http.MethodPost {
				path += "/" + string(pool.Name)
				body = `{"state":"draining","capacityPolicy":{"maxSandboxes":1}}`
			}
			if method == http.MethodGet {
				body = ""
			}
			request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+testPlatformToken)
			request.Header.Set("X-SecondBox-Tenant-Ref", "secondbox")
			request.Header.Set("X-SecondBox-Subject-Ref", "secondbox-admin")
			if method != http.MethodGet {
				request.Header.Set("Content-Type", "application/json")
			}
			if method == http.MethodPatch {
				request.Header.Set("If-Match", fmt.Sprintf(`"revision-%d"`, pool.Revision))
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			content, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if method == http.MethodGet {
				if response.StatusCode != http.StatusOK {
					t.Fatalf("GET = %d: %s", response.StatusCode, content)
				}
				var document map[string]any
				if err := json.Unmarshal(content, &document); err != nil {
					t.Fatal(err)
				}
				if _, exists := document["capacityPolicy"]; exists {
					t.Fatal("pool response retains unused capacity policy")
				}
			} else if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(content), `"code":"invalid_request"`) {
				t.Fatalf("retired policy accepted or not diagnosed: %d %s", response.StatusCode, content)
			}
		})
	}
}
