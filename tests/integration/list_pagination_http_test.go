package integration_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type listPaginationPage struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"nextCursor,omitempty"`
}

func TestCanonicalListEndpointsTraverseStableOpaqueCursorPages(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	suffix := fmt.Sprintf("pagination-%d", integrationIdentitySequence.Add(1))

	for index := 0; index < 3; index++ {
		_, err := createFixtureProject(t, controlPlane,
			t.Context(),
			admin,
			fixtureCreateProjectRequest{Name: fmt.Sprintf("%s-project-%d", suffix, index)},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	accountProject, err := createFixtureProject(t, controlPlane,
		t.Context(),
		admin,
		fixtureCreateProjectRequest{Name: suffix + "-accounts"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		_, err := createFixtureServiceAccount(t, controlPlane,
			t.Context(),
			admin,
			accountProject.ID,
			fixtureCreateServiceAccountRequest{
				Name:          fmt.Sprintf("%s-account-%d", suffix, index),
				Scopes:        []string{"sandbox:read"},
				ProfileGrants: []string{},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	keyProject, keyAccount, _ := createProjectAccountAndCredential(t, controlPlane, admin, suffix+"-keys")
	for index := 0; index < 3; index++ {
		_, err := createFixtureAPIKey(t, controlPlane,
			t.Context(),
			admin,
			keyProject.ID,
			keyAccount.ID,
			fixtureCreateAPIKeyRequest{
				Name:   fmt.Sprintf("%s-key-%d", suffix, index),
				Scopes: []string{"sandbox:read"},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	profileNames := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		profileName := fmt.Sprintf("%s-profile-%d", suffix, index)
		profile, err := controlPlane.CreateProfile(
			t.Context(),
			admin,
			contracts.CreateProfileRequest{Name: profileName, Spec: testProfileSpec(1000)},
		)
		if err != nil {
			t.Fatal(err)
		}
		profileNames = append(profileNames, profile.Name)
	}

	poolNames := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		poolName := fmt.Sprintf("%s-pool-%d", suffix, index)
		pool, err := controlPlane.CreateRunnerPool(
			t.Context(),
			admin,
			contracts.CreateRunnerPoolRequest{
				Name:           poolName,
				State:          contracts.RunnerPoolStateReady,
				Architectures:  []string{"amd64"},
				Capabilities:   []string{"compute"},
				CapacityPolicy: map[string]int64{"maximumInstances": 4},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		poolNames = append(poolNames, pool.Name)
	}

	runnerPoolName := suffix + "-runner-pool"
	if _, err := controlPlane.CreateRunnerPool(
		t.Context(),
		admin,
		contracts.CreateRunnerPoolRequest{
			Name:           runnerPoolName,
			State:          contracts.RunnerPoolStateReady,
			Architectures:  []string{"amd64"},
			Capabilities:   []string{"compute"},
			CapacityPolicy: map[string]int64{"maximumInstances": 4},
		},
	); err != nil {
		t.Fatal(err)
	}
	runnerIDs := seedPaginationRunners(t, runnerPoolName, suffix)

	sandboxProject, sandboxAccount, sandboxCredential := createProjectAccountAndCredential(
		t,
		controlPlane,
		admin,
		suffix+"-sandboxes",
	)
	sandboxProfile := createGrantedProfile(
		t,
		controlPlane,
		databaseStore,
		admin,
		sandboxAccount,
		"profile-"+suffix+"-sandboxes",
	)
	sandboxPrincipal := authenticateCredential(t, controlPlane, sandboxCredential)
	sandboxIDs := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		sandbox, _, err := controlPlane.CreateSandbox(
			t.Context(),
			sandboxPrincipal,
			fmt.Sprintf("%s-sandbox-request-%d", suffix, index),
			contracts.CreateSandboxRequest{Profile: sandboxProfile.Name, Metadata: map[string]string{}},
		)
		if err != nil {
			t.Fatal(err)
		}
		sandboxIDs = append(sandboxIDs, sandbox.ID)
	}
	if sandboxPrincipal.TenantRef != sandboxProject.ID {
		t.Fatalf("Sandbox pagination Principal project = %q, want %q", sandboxPrincipal.TenantRef, sandboxProject.ID)
	}

	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)

	testCases := []struct {
		name       string
		path       string
		credential string
		keyField   string
		expected   []string
	}{
		{name: "profiles", path: "/v1/profiles", credential: "bootstrap-administrator-secret", keyField: "name", expected: profileNames},
		{name: "runner pools", path: "/v1/runner-pools", credential: "bootstrap-administrator-secret", keyField: "name", expected: poolNames},
		{
			name: "runners", credential: "bootstrap-administrator-secret", keyField: "id",
			path: "/v1/runners?pool=" + url.QueryEscape(runnerPoolName), expected: runnerIDs,
		},
		{name: "sandboxes", path: "/v1/sandboxes", credential: sandboxCredential, keyField: "id", expected: sandboxIDs},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assertStableListPagination(
				t,
				server.URL,
				testCase.path,
				testCase.credential,
				testCase.keyField,
				testCase.expected,
			)
		})
	}
}

func seedPaginationRunners(t *testing.T, poolName string, suffix string) []string {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	createdAt := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	runnerIDs := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		runnerID := fmt.Sprintf("runner-%s-%d", suffix, index)
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO secondbox.runners (
				id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
				protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
				software_version,active_connection_id,last_sequence,drain_phase,
				reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
				sandbox_start_p95_milliseconds,last_seen_at,revision,
				created_at,updated_at
			) VALUES (
				$1,$2,$3,'offline','["amd64"]','["compute"]','{"instances":0}','["1"]',
				1,1,'1.0.0','',0,'active','{}','[]',0,0,NULL,1,$4,$4
			)`,
			runnerID,
			poolName,
			fmt.Sprintf("%s-runner-%d", suffix, index),
			createdAt,
		); err != nil {
			t.Fatal(err)
		}
		runnerIDs = append(runnerIDs, runnerID)
	}
	return runnerIDs
}

func assertStableListPagination(
	t *testing.T,
	baseURL string,
	path string,
	credential string,
	keyField string,
	expected []string,
) {
	t.Helper()
	seen := map[string]bool{}
	cursor := ""
	for pageNumber := 0; pageNumber < 1000; pageNumber++ {
		pageURL, err := url.Parse(baseURL + path)
		if err != nil {
			t.Fatal(err)
		}
		query := pageURL.Query()
		query.Set("limit", "2")
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		pageURL.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, pageURL.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		setPlatformAuthorization(t, request, credential)
		response := doHTTP(t, request)
		assertHTTPStatus(t, response, http.StatusOK)
		var page listPaginationPage
		decodeHTTPJSON(t, response, &page)
		if len(page.Items) == 0 && page.NextCursor != nil {
			t.Fatalf("List page %d returned an empty page with nextCursor %q", pageNumber, *page.NextCursor)
		}
		for _, item := range page.Items {
			itemKey, ok := item[keyField].(string)
			if !ok || itemKey == "" {
				t.Fatalf("List page item has no %q string: %#v", keyField, item)
			}
			if seen[itemKey] {
				t.Fatalf("List pagination returned duplicate %q", itemKey)
			}
			seen[itemKey] = true
		}
		if page.NextCursor == nil {
			break
		}
		if *page.NextCursor == "" || *page.NextCursor == cursor {
			t.Fatalf("List page %d returned a non-progressing cursor %q", pageNumber, *page.NextCursor)
		}
		cursor = *page.NextCursor
		if pageNumber == 999 {
			t.Fatal("List pagination did not terminate")
		}
	}
	sort.Strings(expected)
	for _, itemKey := range expected {
		if !seen[itemKey] {
			t.Errorf("List pagination did not reach seeded item %q", itemKey)
		}
	}

	invalidURL, err := url.Parse(baseURL + path)
	if err != nil {
		t.Fatal(err)
	}
	invalidQuery := invalidURL.Query()
	invalidQuery.Set("limit", "2")
	invalidQuery.Set("cursor", "definitely-not-a-valid-opaque-cursor")
	invalidURL.RawQuery = invalidQuery.Encode()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, invalidURL.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, request, credential)
	response := doHTTP(t, request)
	assertHTTPStatus(t, response, http.StatusBadRequest)
	if contentType := response.Header.Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("invalid cursor Content-Type = %q", contentType)
	}
	var problem contracts.Problem
	decodeHTTPJSON(t, response, &problem)
	if problem.Code != "invalid_request" || problem.Status != http.StatusBadRequest {
		t.Fatalf("invalid cursor Problem = %#v", problem)
	}
}
