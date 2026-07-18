package runtimecontext

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	oauthpkg "agentcy/internal/oauth"
	"agentcy/internal/registry"
	"agentcy/internal/registry/registrytest"
)

var errRuntimeContextSecretStoreCalled = errors.New("runtime context build should not read oauth secrets")

type failingRuntimeContextOAuthSecretStore struct{}

func (f failingRuntimeContextOAuthSecretStore) PutOAuthTokenSecret(registry.OAuthTokenSecretKey, registry.OAuthTokenSecrets) error {
	return errRuntimeContextSecretStoreCalled
}

func (f failingRuntimeContextOAuthSecretStore) GetOAuthTokenSecret(registry.OAuthTokenSecretKey) (registry.OAuthTokenSecrets, error) {
	return registry.OAuthTokenSecrets{}, errRuntimeContextSecretStoreCalled
}

func (f failingRuntimeContextOAuthSecretStore) DeleteOAuthTokenSecret(registry.OAuthTokenSecretKey) error {
	return errRuntimeContextSecretStoreCalled
}

type fakeRuntimeContextStore struct {
	metadata        map[string][]*registry.OAuthTokenMetadata
	userMetadata    map[string]map[string][]*registry.UserOAuthConnectionMetadata
	permissions     map[string]map[string]string
	userPermissions map[string]map[string]map[string]string
	deniedUsers     map[string]bool
	filters         map[string]map[string]string
}

func (s *fakeRuntimeContextStore) GetAgentScopedOAuthMetadataForCompartment(_, _, service string) ([]*registry.OAuthTokenMetadata, error) {
	return append([]*registry.OAuthTokenMetadata(nil), s.metadata[service]...), nil
}

func (s *fakeRuntimeContextStore) GetOAuthPermissionMapForCompartment(_, _, service string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range s.permissions[service] {
		out[k] = v
	}
	return out, nil
}

func (s *fakeRuntimeContextStore) ListUserOAuthConnectionMetadata(userID, service string) ([]*registry.UserOAuthConnectionMetadata, error) {
	return append([]*registry.UserOAuthConnectionMetadata(nil), s.userMetadata[userID][service]...), nil
}

func (s *fakeRuntimeContextStore) GetUserOAuthPermissionMap(userID, service string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range s.userPermissions[userID][service] {
		out[k] = v
	}
	return out, nil
}

func (s *fakeRuntimeContextStore) IsUserAgentImpersonationDenied(userID, _ string) (bool, error) {
	return s.deniedUsers[userID], nil
}

func (s *fakeRuntimeContextStore) PlaceCredentialFilterMap(_, service string) (map[string]string, bool, error) {
	out := map[string]string{}
	for k, v := range s.filters[service] {
		out[k] = v
	}
	return out, len(out) > 0, nil
}

func freshVerifiedActor(userID string) runtimecontextActorInput {
	return runtimecontextActorInput{
		Actor: VerifiedActorContext{
			Principal:      "slack:user:U_ALICE",
			PlatformUserID: userID,
			SessionID:      "session-1",
			TurnContextID:  "turn-1",
			RequestID:      "request-1",
			Verified:       true,
			ExpiresAt:      time.Now().Add(time.Hour),
		},
		CurrentSessionID:     "session-1",
		CurrentTurnContextID: "turn-1",
		CurrentRequestID:     "request-1",
	}
}

type runtimecontextActorInput struct {
	Actor                VerifiedActorContext
	CurrentSessionID     string
	CurrentTurnContextID string
	CurrentRequestID     string
}

func applyActorInput(req *BuildRequest, input runtimecontextActorInput) {
	req.Actor = input.Actor
	req.CurrentSessionID = input.CurrentSessionID
	req.CurrentTurnContextID = input.CurrentTurnContextID
	req.CurrentRequestID = input.CurrentRequestID
}

func TestBuildProjectsAgentOwnedServiceContexts(t *testing.T) {
	store := &fakeRuntimeContextStore{
		metadata: map[string][]*registry.OAuthTokenMetadata{
			registry.OAuthServiceJira: {{
				Service:      registry.OAuthServiceJira,
				MetadataJSON: `{"mode":"read_write","selectedSite":{"cloudId":"cloud-1","url":"https://example.atlassian.net","name":"Example"}}`,
			}},
			registry.OAuthServiceNotion: {{
				Service:      registry.OAuthServiceNotion,
				MetadataJSON: `{"workspace_id":"ws-1","workspace_name":"Roadmap","bot_id":"bot-1","owner_type":"user","owner_user_id":"user-1","owner_user_name":"Ada"}`,
			}},
			registry.OAuthServiceGoogleWorkspace: {{
				Service: registry.OAuthServiceGoogleWorkspace,
			}},
			registry.OAuthServiceTempo: {{
				Service:      registry.OAuthServiceTempo,
				MetadataJSON: `{"account_id":"tempo-account-1"}`,
			}},
			registry.OAuthServiceGitLab: {{
				Service:      registry.OAuthServiceGitLab,
				MetadataJSON: `{"host":"gitlab.example.com","base_url":"https://gitlab.example.com","username":"ada","account_id":"42"}`,
			}},
		},
		permissions: map[string]map[string]string{
			registry.OAuthServiceGoogleWorkspace: {
				"gmail":    registry.OAuthAccessRead,
				"calendar": registry.OAuthAccessWrite,
				"drive":    registry.OAuthAccessNone,
				"docs":     registry.OAuthAccessNone,
				"sheets":   registry.OAuthAccessNone,
				"slides":   registry.OAuthAccessNone,
			},
		},
	}

	projection, err := Build(context.Background(), store, BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		EffectivePolicy: registry.CredentialPolicyAgentIdentity,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertProjectedJSONField(t, projection, PathJiraContext, "selectedSite.cloudId", "cloud-1")
	assertProjectedJSONField(t, projection, PathNotionContext, "apiVersion", oauthpkg.NotionAPIVersion)
	assertProjectedJSONField(t, projection, PathGoogleWorkspacePolicy, "applications.calendar", registry.OAuthAccessWrite)
	assertProjectedJSONField(t, projection, PathTempoContext, "accountId", "tempo-account-1")
	assertProjectedJSONField(t, projection, PathGitLabContext, "baseUrl", "https://gitlab.example.com")
	assertProjectedJSONFieldAbsent(t, projection, PathJiraContext, "selectedSite.url")
	assertProjectedJSONFieldAbsent(t, projection, PathJiraContext, "selectedSite.name")
	assertProjectedJSONFieldAbsent(t, projection, PathNotionContext, "workspaceName")
	assertProjectedJSONFieldAbsent(t, projection, PathNotionContext, "owner")
	assertProjectedJSONFieldAbsent(t, projection, PathGitLabContext, "username")
	assertProjectedJSONFieldAbsent(t, projection, PathGitLabContext, "email")
	assertProjectedJSONFieldAbsent(t, projection, PathGitLabContext, "accountId")
	if projection.PartialRolloutVersion != PartialRolloutVersionServices {
		t.Fatalf("partial rollout version = %q", projection.PartialRolloutVersion)
	}
}

func TestBuildPolicyNoneOmitsEveryRuntimeContextFile(t *testing.T) {
	projection, err := Build(context.Background(), &fakeRuntimeContextStore{}, BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		EffectivePolicy: registry.CredentialPolicyNone,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(projection.Files) != 0 {
		t.Fatalf("policy none emitted files: %#v", projection.Files)
	}
	for _, path := range AllProjectionPaths() {
		if !containsString(projection.OmittedPaths, path) {
			t.Fatalf("policy none omitted paths missing %s: %#v", path, projection.OmittedPaths)
		}
	}
}

func TestBuildProjectionDoesNotLeakTokenSecrets(t *testing.T) {
	store := &fakeRuntimeContextStore{
		metadata: map[string][]*registry.OAuthTokenMetadata{
			registry.OAuthServiceGitHub: {{
				Service:      registry.OAuthServiceGitHub,
				TokenType:    "GitHubAppInstallation",
				MetadataJSON: `{"host":"github.com","installation_id":42,"account_login":"owner","repository_selection":"selected","accessible_repos":["owner/repo"],"repo_list_complete":true}`,
			}},
			registry.OAuthServiceJira: {{
				Service:      registry.OAuthServiceJira,
				MetadataJSON: `{"mode":"read","selectedSite":{"cloudId":"cloud-1","url":"https://example.atlassian.net","name":"Example"}}`,
			}},
			registry.OAuthServiceNotion: {{
				Service:      registry.OAuthServiceNotion,
				MetadataJSON: `{"workspace_name":"Roadmap","owner_user_email":"owner@example.com"}`,
			}},
			registry.OAuthServiceGitLab: {{
				Service:      registry.OAuthServiceGitLab,
				MetadataJSON: `{"host":"gitlab.example.com","base_url":"https://gitlab.example.com","email":"owner@example.com"}`,
			}},
		},
	}
	projection, err := Build(context.Background(), store, BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		EffectivePolicy: registry.CredentialPolicyAgentIdentity,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, file := range projection.Files {
		if strings.Contains(file.Content, "secret-") || strings.Contains(file.Content, "owner@example.com") {
			t.Fatalf("%s leaked token or account metadata: %s", file.Path, file.Content)
		}
	}
}

func TestBuildWithRegistryStoreDoesNotReadOAuthSecrets(t *testing.T) {
	store := registrytest.Open(t)
	agent := &registry.Agent{
		ID:     "rtctxsecret0001",
		Name:   "runtime context secret test",
		Model:  "anthropic:claude-sonnet-4-5",
		Status: registry.StatusStopped,
	}
	if err := store.CreateAgent(agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := store.CreateUser(&registry.User{
		ID:    "user-alice",
		Email: "alice@example.com",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := store.UpsertOAuthToken(&registry.OAuthToken{
		AgentID:         agent.ID,
		Service:         registry.OAuthServiceGitHub,
		InstallationKey: "installation:1001",
		AccessToken:     "secret-github-access",
		RefreshToken:    "secret-github-refresh",
		TokenType:       "GitHubAppInstallation",
		MetadataJSON:    `{"host":"github.com","installation_id":1001,"account_login":"owner","repository_selection":"selected","accessible_repos":["owner/repo"],"repo_list_complete":true}`,
	}); err != nil {
		t.Fatalf("upsert github token: %v", err)
	}
	if err := store.UpsertUserOAuthConnection(&registry.UserOAuthConnection{
		UserID:       "user-alice",
		Service:      registry.OAuthServiceNotion,
		AccessToken:  "secret-user-notion-access",
		RefreshToken: "secret-user-notion-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
		MetadataJSON: `{"workspace_name":"Alice Workspace","owner_user_email":"alice@example.com"}`,
	}); err != nil {
		t.Fatalf("upsert user notion connection: %v", err)
	}

	store.SetOAuthTokenSecretStore(failingRuntimeContextOAuthSecretStore{})

	agentProjection, err := Build(context.Background(), store, BuildRequest{
		AgentID:         agent.ID,
		CompartmentID:   "cmp-1",
		EffectivePolicy: registry.CredentialPolicyAgentIdentity,
	})
	if err != nil {
		t.Fatalf("Build agent identity with failing secret store: %v", err)
	}
	githubContext, ok := agentProjection.FileContent(PathGitHubContext)
	if !ok || !strings.Contains(githubContext, "owner/repo") {
		t.Fatalf("missing projected GitHub context: %#v", agentProjection)
	}

	req := BuildRequest{
		AgentID:              agent.ID,
		CompartmentID:        "cmp-1",
		EffectivePolicy:      registry.CredentialPolicyUserDelegated,
		CurrentSessionID:     "session-1",
		CurrentTurnContextID: "turn-1",
		CurrentRequestID:     "request-1",
		Actor: VerifiedActorContext{
			Principal:      "slack:user:U_ALICE",
			PlatformUserID: "user-alice",
			SessionID:      "session-1",
			TurnContextID:  "turn-1",
			RequestID:      "request-1",
			Verified:       true,
			ExpiresAt:      time.Now().Add(time.Hour),
		},
	}
	delegatedProjection, err := Build(context.Background(), store, req)
	if err != nil {
		t.Fatalf("Build delegated with failing secret store: %v", err)
	}
	assertProjectedJSONField(t, delegatedProjection, PathNotionContext, "apiVersion", oauthpkg.NotionAPIVersion)
	for _, file := range append(agentProjection.Files, delegatedProjection.Files...) {
		if strings.Contains(file.Content, "secret-") || strings.Contains(file.Content, "alice@example.com") {
			t.Fatalf("%s leaked secret or account metadata: %s", file.Path, file.Content)
		}
	}
}

func TestBuildOmitsNonGitHubContextsForDelegatedPolicy(t *testing.T) {
	store := &fakeRuntimeContextStore{
		metadata: map[string][]*registry.OAuthTokenMetadata{
			registry.OAuthServiceJira: {{
				Service:      registry.OAuthServiceJira,
				MetadataJSON: `{"mode":"read","selectedSite":{"cloudId":"cloud-1","url":"https://example.atlassian.net","name":"Example"}}`,
			}},
		},
	}
	projection, err := Build(context.Background(), store, BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		EffectivePolicy: registry.CredentialPolicyUserDelegated,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := projection.FileContent(PathJiraContext); ok {
		t.Fatal("delegated policy should omit agent-owned Jira context without a verified actor")
	}
	if !containsString(projection.OmittedPaths, PathJiraContext) || !containsString(projection.MigratedServicePaths, PathJiraContext) {
		t.Fatalf("delegated projection did not mark Jira omitted/migrated: %#v", projection)
	}
}

func TestBuildProjectsUserDelegatedServiceContextsFromVerifiedActor(t *testing.T) {
	store := &fakeRuntimeContextStore{
		metadata: map[string][]*registry.OAuthTokenMetadata{
			registry.OAuthServiceJira: {{
				Service:      registry.OAuthServiceJira,
				MetadataJSON: `{"mode":"read","selectedSite":{"cloudId":"agent-cloud"}}`,
			}},
		},
		userMetadata: map[string]map[string][]*registry.UserOAuthConnectionMetadata{
			"user-alice": {
				registry.OAuthServiceJira: {{
					Service:      registry.OAuthServiceJira,
					MetadataJSON: `{"mode":"read_write","selectedSite":{"cloudId":"user-cloud","url":"https://user.atlassian.net","name":"User"}}`,
				}},
				registry.OAuthServiceNotion: {{
					Service:      registry.OAuthServiceNotion,
					MetadataJSON: `{"workspace_name":"User Roadmap","owner_user_email":"owner@example.com"}`,
				}},
				registry.OAuthServiceGoogleWorkspace: {{
					Service: registry.OAuthServiceGoogleWorkspace,
				}},
				registry.OAuthServiceTempo: {{
					Service:      registry.OAuthServiceTempo,
					MetadataJSON: `{"account_id":"tempo-user-account"}`,
				}},
				registry.OAuthServiceGitLab: {{
					Service:      registry.OAuthServiceGitLab,
					MetadataJSON: `{"host":"gitlab.example.com","base_url":"https://gitlab.example.com","username":"alice","email":"alice@example.com","account_id":"42"}`,
				}},
			},
		},
		userPermissions: map[string]map[string]map[string]string{
			"user-alice": {
				registry.OAuthServiceGoogleWorkspace: {
					"gmail":    registry.OAuthAccessWrite,
					"calendar": registry.OAuthAccessRead,
				},
			},
		},
	}
	req := BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		EffectivePolicy: registry.CredentialPolicyUserDelegated,
	}
	actor := freshVerifiedActor("user-alice")
	applyActorInput(&req, actor)

	projection, err := Build(context.Background(), store, req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertProjectedJSONField(t, projection, PathJiraContext, "selectedSite.cloudId", "user-cloud")
	assertProjectedJSONField(t, projection, PathNotionContext, "apiVersion", oauthpkg.NotionAPIVersion)
	assertProjectedJSONField(t, projection, PathGoogleWorkspacePolicy, "applications.gmail", registry.OAuthAccessWrite)
	assertProjectedJSONField(t, projection, PathGoogleWorkspacePolicy, "applications.calendar", registry.OAuthAccessRead)
	assertProjectedJSONField(t, projection, PathTempoContext, "accountId", "tempo-user-account")
	assertProjectedJSONField(t, projection, PathGitLabContext, "baseUrl", "https://gitlab.example.com")
	assertProjectedJSONFieldAbsent(t, projection, PathJiraContext, "selectedSite.url")
	assertProjectedJSONFieldAbsent(t, projection, PathNotionContext, "workspaceName")
	assertProjectedJSONFieldAbsent(t, projection, PathNotionContext, "owner")
	assertProjectedJSONFieldAbsent(t, projection, PathGitLabContext, "username")
	assertProjectedJSONFieldAbsent(t, projection, PathGitLabContext, "email")
	assertProjectedJSONFieldAbsent(t, projection, PathGitLabContext, "accountId")
}

func TestBuildProjectsUserDelegatedMissingConnectionConnectURLs(t *testing.T) {
	store := &fakeRuntimeContextStore{}
	req := BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		PublicBaseURL:   "https://agentcy.example.com/",
		EffectivePolicy: registry.CredentialPolicyUserDelegated,
	}
	applyActorInput(&req, freshVerifiedActor("user-alice"))

	projection, err := Build(context.Background(), store, req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, tc := range []struct {
		path    string
		service string
	}{
		{PathJiraContext, registry.OAuthServiceJira},
		{PathNotionContext, registry.OAuthServiceNotion},
		{PathGoogleWorkspacePolicy, registry.OAuthServiceGoogleWorkspace},
		{PathTempoContext, registry.OAuthServiceTempo},
		{PathGitLabContext, registry.OAuthServiceGitLab},
	} {
		assertProjectedJSONField(t, projection, tc.path, "reason", "user_connection_required")
		assertProjectedJSONField(t, projection, tc.path, "details.service", tc.service)
		assertProjectedJSONField(t, projection, tc.path, "connectUrl", "https://agentcy.example.com/#/settings/connections?oauth_service="+tc.service)
		assertProjectedJSONFieldAbsent(t, projection, tc.path, "access_token")
	}
}

func TestBuildOmitsUserDelegatedContextsWithoutFreshVerifiedActor(t *testing.T) {
	store := &fakeRuntimeContextStore{
		userMetadata: map[string]map[string][]*registry.UserOAuthConnectionMetadata{
			"user-alice": {
				registry.OAuthServiceJira: {{
					Service:      registry.OAuthServiceJira,
					MetadataJSON: `{"mode":"read","selectedSite":{"cloudId":"user-cloud"}}`,
				}},
			},
		},
	}
	base := BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		EffectivePolicy: registry.CredentialPolicyUserDelegated,
	}
	fresh := freshVerifiedActor("user-alice")
	tests := []struct {
		name   string
		mutate func(*BuildRequest)
	}{
		{name: "missing actor", mutate: func(*BuildRequest) {}},
		{name: "missing platform user", mutate: func(req *BuildRequest) {
			applyActorInput(req, fresh)
			req.Actor.PlatformUserID = ""
		}},
		{name: "unverified actor", mutate: func(req *BuildRequest) {
			applyActorInput(req, fresh)
			req.Actor.Verified = false
		}},
		{name: "system actor", mutate: func(req *BuildRequest) {
			applyActorInput(req, fresh)
			req.Actor.Principal = "system:user:EVENT"
		}},
		{name: "stale session", mutate: func(req *BuildRequest) {
			applyActorInput(req, fresh)
			req.Actor.SessionID = "old-session"
		}},
		{name: "stale request", mutate: func(req *BuildRequest) {
			applyActorInput(req, fresh)
			req.Actor.RequestID = "old-request"
		}},
		{name: "expired actor", mutate: func(req *BuildRequest) {
			applyActorInput(req, fresh)
			req.Actor.ExpiresAt = time.Now().Add(-time.Minute)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mutate(&req)
			projection, err := Build(context.Background(), store, req)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if _, ok := projection.FileContent(PathJiraContext); ok {
				t.Fatalf("projected delegated Jira context for %s", tc.name)
			}
			for _, file := range projection.Files {
				if strings.Contains(file.Content, "user_connection_required") || strings.Contains(file.Content, "connectUrl") {
					t.Fatalf("%s leaked connect projection for stale actor: %#v", tc.name, projection.Files)
				}
			}
			for _, path := range []string{PathJiraContext, PathNotionContext, PathGoogleWorkspacePolicy, PathTempoContext, PathGitLabContext} {
				if !containsString(projection.OmittedPaths, path) {
					t.Fatalf("%s omitted paths missing %s: %#v", tc.name, path, projection.OmittedPaths)
				}
			}
		})
	}
}

func TestBuildOmitsUserDelegatedContextsWhenImpersonationDenied(t *testing.T) {
	store := &fakeRuntimeContextStore{
		userMetadata: map[string]map[string][]*registry.UserOAuthConnectionMetadata{
			"user-alice": {
				registry.OAuthServiceJira: {{
					Service:      registry.OAuthServiceJira,
					MetadataJSON: `{"mode":"read","selectedSite":{"cloudId":"user-cloud"}}`,
				}},
			},
		},
		deniedUsers: map[string]bool{"user-alice": true},
	}
	req := BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		EffectivePolicy: registry.CredentialPolicyUserDelegated,
	}
	applyActorInput(&req, freshVerifiedActor("user-alice"))
	projection, err := Build(context.Background(), store, req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := projection.FileContent(PathJiraContext); ok {
		t.Fatal("projected delegated Jira context for denied impersonation")
	}
	if !containsString(projection.OmittedPaths, PathJiraContext) {
		t.Fatalf("denied actor omitted paths missing Jira: %#v", projection.OmittedPaths)
	}
}

func TestBuildOmitsExpiredOAuthMetadata(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	store := &fakeRuntimeContextStore{
		metadata: map[string][]*registry.OAuthTokenMetadata{
			registry.OAuthServiceGitHub: {{
				Service:      registry.OAuthServiceGitHub,
				TokenType:    "GitHubAppInstallation",
				Expiry:       expired,
				MetadataJSON: `{"installation_id":42,"account_login":"owner","accessible_repos":["owner/repo"]}`,
			}},
			registry.OAuthServiceJira: {{
				Service:      registry.OAuthServiceJira,
				Expiry:       expired,
				MetadataJSON: `{"mode":"read","selectedSite":{"cloudId":"agent-cloud"}}`,
			}},
		},
		userMetadata: map[string]map[string][]*registry.UserOAuthConnectionMetadata{
			"user-alice": {
				registry.OAuthServiceJira: {{
					Service:      registry.OAuthServiceJira,
					Expiry:       expired,
					MetadataJSON: `{"mode":"read","selectedSite":{"cloudId":"user-cloud"}}`,
				}},
			},
		},
	}
	agentProjection, err := Build(context.Background(), store, BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		EffectivePolicy: registry.CredentialPolicyAgentIdentity,
	})
	if err != nil {
		t.Fatalf("Build agent projection: %v", err)
	}
	if _, ok := agentProjection.FileContent(PathGitHubContext); ok {
		t.Fatal("projected expired GitHub metadata")
	}
	if _, ok := agentProjection.FileContent(PathJiraContext); ok {
		t.Fatal("projected expired agent Jira metadata")
	}

	req := BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		EffectivePolicy: registry.CredentialPolicyUserDelegated,
	}
	applyActorInput(&req, freshVerifiedActor("user-alice"))
	delegatedProjection, err := Build(context.Background(), store, req)
	if err != nil {
		t.Fatalf("Build delegated projection: %v", err)
	}
	if _, ok := delegatedProjection.FileContent(PathJiraContext); ok {
		t.Fatal("projected expired delegated Jira metadata")
	}
}

func TestBuildNormalizesOmittedPathsForMissingExpiredAndIncompleteGrants(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	tests := []struct {
		name      string
		store     *fakeRuntimeContextStore
		req       BuildRequest
		wantOmit  []string
		wantFiles []string
	}{
		{
			name: "agent owned missing grant omits every absent service path",
			store: &fakeRuntimeContextStore{
				metadata:    map[string][]*registry.OAuthTokenMetadata{},
				permissions: map[string]map[string]string{},
			},
			req: BuildRequest{
				AgentID:         "agent-1",
				CompartmentID:   "cmp-1",
				EffectivePolicy: registry.CredentialPolicyAgentIdentity,
			},
			wantOmit: []string{
				PathGitHubContext,
				PathJiraContext,
				PathNotionContext,
				PathGoogleWorkspacePolicy,
				PathTempoContext,
				PathGitLabContext,
			},
		},
		{
			name: "agent owned expired and incomplete grants omit instead of projecting stale files",
			store: &fakeRuntimeContextStore{
				metadata: map[string][]*registry.OAuthTokenMetadata{
					registry.OAuthServiceJira: {{
						Service:      registry.OAuthServiceJira,
						MetadataJSON: `{"mode":"read"}`,
					}},
					registry.OAuthServiceGoogleWorkspace: {{
						Service: registry.OAuthServiceGoogleWorkspace,
					}},
					registry.OAuthServiceTempo: {{
						Service:      registry.OAuthServiceTempo,
						Expiry:       expired,
						MetadataJSON: `{"account_id":"tempo-expired"}`,
					}},
				},
				permissions: map[string]map[string]string{
					registry.OAuthServiceGoogleWorkspace: {
						"gmail":    registry.OAuthAccessNone,
						"calendar": registry.OAuthAccessNone,
					},
				},
			},
			req: BuildRequest{
				AgentID:         "agent-1",
				CompartmentID:   "cmp-1",
				EffectivePolicy: registry.CredentialPolicyAgentIdentity,
			},
			wantOmit: []string{PathJiraContext, PathGoogleWorkspacePolicy, PathTempoContext},
		},
		{
			name: "delegated missing expired and incomplete grants omit without public connect url",
			store: &fakeRuntimeContextStore{
				userMetadata: map[string]map[string][]*registry.UserOAuthConnectionMetadata{
					"user-alice": {
						registry.OAuthServiceJira: {{
							Service:      registry.OAuthServiceJira,
							MetadataJSON: `{"mode":"read"}`,
						}},
						registry.OAuthServiceGoogleWorkspace: {{
							Service: registry.OAuthServiceGoogleWorkspace,
						}},
						registry.OAuthServiceTempo: {{
							Service:      registry.OAuthServiceTempo,
							Expiry:       expired,
							MetadataJSON: `{"account_id":"tempo-expired"}`,
						}},
					},
				},
				userPermissions: map[string]map[string]map[string]string{
					"user-alice": {
						registry.OAuthServiceGoogleWorkspace: {
							"gmail": registry.OAuthAccessNone,
						},
					},
				},
			},
			req: func() BuildRequest {
				req := BuildRequest{
					AgentID:         "agent-1",
					CompartmentID:   "cmp-1",
					EffectivePolicy: registry.CredentialPolicyUserDelegated,
				}
				applyActorInput(&req, freshVerifiedActor("user-alice"))
				return req
			}(),
			wantOmit: []string{PathJiraContext, PathGoogleWorkspacePolicy, PathTempoContext, PathNotionContext, PathGitLabContext},
		},
		{
			name:  "delegated missing grants use connect prompts only when public url is available",
			store: &fakeRuntimeContextStore{},
			req: func() BuildRequest {
				req := BuildRequest{
					AgentID:         "agent-1",
					CompartmentID:   "cmp-1",
					PublicBaseURL:   "https://agentcy.example.com",
					EffectivePolicy: registry.CredentialPolicyUserDelegated,
				}
				applyActorInput(&req, freshVerifiedActor("user-alice"))
				return req
			}(),
			wantFiles: []string{PathJiraContext, PathNotionContext, PathGoogleWorkspacePolicy, PathTempoContext, PathGitLabContext},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			projection, err := Build(context.Background(), tc.store, tc.req)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			for _, path := range tc.wantOmit {
				if _, ok := projection.FileContent(path); ok {
					t.Fatalf("%s projected file that should be omitted: %#v", path, projection)
				}
				if !containsString(projection.OmittedPaths, path) {
					t.Fatalf("omitted paths missing %s: %#v", path, projection.OmittedPaths)
				}
			}
			for _, path := range tc.wantFiles {
				assertProjectedJSONField(t, projection, path, "reason", "user_connection_required")
				if containsString(projection.OmittedPaths, path) {
					t.Fatalf("%s had both connect prompt and omitted path: %#v", path, projection)
				}
			}
		})
	}
}

func TestBuildNarrowsGoogleWorkspacePolicyWithPlaceFilters(t *testing.T) {
	store := &fakeRuntimeContextStore{
		metadata: map[string][]*registry.OAuthTokenMetadata{
			registry.OAuthServiceGoogleWorkspace: {{Service: registry.OAuthServiceGoogleWorkspace}},
		},
		permissions: map[string]map[string]string{
			registry.OAuthServiceGoogleWorkspace: {
				"gmail": registry.OAuthAccessWrite,
				"drive": registry.OAuthAccessWrite,
			},
		},
		filters: map[string]map[string]string{
			registry.OAuthServiceGoogleWorkspace: {
				"gmail": registry.OAuthAccessRead,
				"drive": registry.OAuthAccessNone,
			},
		},
	}
	projection, err := Build(context.Background(), store, BuildRequest{
		AgentID:         "agent-1",
		CompartmentID:   "cmp-1",
		PlaceID:         "plc-1",
		EffectivePolicy: registry.CredentialPolicyAgentIdentity,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertProjectedJSONField(t, projection, PathGoogleWorkspacePolicy, "applications.gmail", registry.OAuthAccessRead)
	assertProjectedJSONField(t, projection, PathGoogleWorkspacePolicy, "applications.drive", registry.OAuthAccessNone)
}

func assertProjectedJSONField(t *testing.T, projection Projection, path, dotted, want string) {
	t.Helper()
	content, ok := projection.FileContent(path)
	if !ok {
		t.Fatalf("missing projected file %s in %#v", path, projection)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, content)
	}
	var value any = payload
	for _, part := range splitDotted(dotted) {
		next, ok := value.(map[string]any)[part]
		if !ok {
			t.Fatalf("%s missing field %s in %#v", path, dotted, payload)
		}
		value = next
	}
	if got, ok := value.(string); !ok || got != want {
		t.Fatalf("%s field %s = %#v, want %q", path, dotted, value, want)
	}
}

func assertProjectedJSONFieldAbsent(t *testing.T, projection Projection, path, dotted string) {
	t.Helper()
	content, ok := projection.FileContent(path)
	if !ok {
		t.Fatalf("missing projected file %s in %#v", path, projection)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, content)
	}
	var value any = payload
	parts := splitDotted(dotted)
	for i, part := range parts {
		object, ok := value.(map[string]any)
		if !ok {
			return
		}
		next, ok := object[part]
		if !ok {
			return
		}
		if i == len(parts)-1 {
			t.Fatalf("%s unexpectedly included field %s in %#v", path, dotted, payload)
		}
		value = next
	}
}

func splitDotted(value string) []string {
	return strings.Split(value, ".")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
