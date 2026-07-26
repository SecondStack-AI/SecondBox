package runtimecontext

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	oauthpkg "agent-manager/internal/oauth"
	"agent-manager/internal/registry"
)

type Store interface {
	GetAgentScopedOAuthMetadataForCompartment(agentID, compartmentID, service string) ([]*registry.OAuthTokenMetadata, error)
	GetOAuthPermissionMapForCompartment(agentID, compartmentID, service string) (map[string]string, error)
	ListUserOAuthConnectionMetadata(userID, service string) ([]*registry.UserOAuthConnectionMetadata, error)
	GetUserOAuthPermissionMap(userID, service string) (map[string]string, error)
	IsUserAgentImpersonationDenied(userID, agentID string) (bool, error)
	PlaceCredentialFilterMap(placeID, service string) (map[string]string, bool, error)
}

func Build(ctx context.Context, store Store, req BuildRequest) (Projection, error) {
	if err := ctx.Err(); err != nil {
		return Projection{}, err
	}
	if store == nil {
		return Projection{}, fmt.Errorf("runtime context store is required")
	}
	projection := Projection{
		EffectivePolicy:       strings.TrimSpace(req.EffectivePolicy),
		PartialRolloutVersion: PartialRolloutVersionServices,
		MigratedServicePaths:  AllProjectionPaths(),
	}
	if strings.TrimSpace(req.EffectivePolicy) == registry.CredentialPolicyNone {
		projection.OmittedPaths = AllProjectionPaths()
		return Canonicalize(projection)
	}
	github, err := buildGitHubContext(store, req)
	if err != nil {
		return Projection{}, err
	}
	if github == "" {
		projection.OmittedPaths = append(projection.OmittedPaths, PathGitHubContext)
	} else {
		projection.Files = append(projection.Files, File{Path: PathGitHubContext, Content: github})
	}
	if strings.TrimSpace(req.EffectivePolicy) == registry.CredentialPolicyUserDelegated {
		if err := buildUserDelegatedContexts(store, req, &projection); err != nil {
			return Projection{}, err
		}
		return Canonicalize(projection)
	}
	builders := []struct {
		path    string
		service string
		build   func(Store, BuildRequest) (string, error)
	}{
		{PathJiraContext, registry.OAuthServiceJira, buildJiraContext},
		{PathNotionContext, registry.OAuthServiceNotion, buildNotionContext},
		{PathGoogleWorkspacePolicy, registry.OAuthServiceGoogleWorkspace, buildGoogleWorkspacePolicy},
		{PathTempoContext, registry.OAuthServiceTempo, buildTempoContext},
		{PathGitLabContext, registry.OAuthServiceGitLab, buildGitLabContext},
	}
	for _, builder := range builders {
		if blocked, err := serviceBlockedByPlaceFilter(store, req.PlaceID, builder.service); err != nil {
			return Projection{}, err
		} else if blocked {
			projection.OmittedPaths = append(projection.OmittedPaths, builder.path)
			continue
		}
		content, err := builder.build(store, req)
		if err != nil {
			return Projection{}, err
		}
		if content == "" {
			projection.OmittedPaths = append(projection.OmittedPaths, builder.path)
			continue
		}
		projection.Files = append(projection.Files, File{Path: builder.path, Content: content})
	}
	return Canonicalize(projection)
}

func firstAgentScopedMetadata(store Store, req BuildRequest, service string) (*registry.OAuthTokenMetadata, error) {
	metadata, err := store.GetAgentScopedOAuthMetadataForCompartment(req.AgentID, req.CompartmentID, service)
	if err != nil {
		return nil, err
	}
	filtered := make([]*registry.OAuthTokenMetadata, 0, len(metadata))
	for _, item := range metadata {
		if item != nil && oauthMetadataActive(item.Expiry) {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].InstallationKey != filtered[j].InstallationKey {
			return filtered[i].InstallationKey < filtered[j].InstallationKey
		}
		if filtered[i].TokenType != filtered[j].TokenType {
			return filtered[i].TokenType < filtered[j].TokenType
		}
		return filtered[i].UpdatedAt.Before(filtered[j].UpdatedAt)
	})
	return filtered[0], nil
}

func firstUserScopedMetadata(store Store, req BuildRequest, service, userID string) (*registry.UserOAuthConnectionMetadata, error) {
	metadata, err := store.ListUserOAuthConnectionMetadata(userID, service)
	if err != nil {
		return nil, err
	}
	filtered := make([]*registry.UserOAuthConnectionMetadata, 0, len(metadata))
	for _, item := range metadata {
		if item != nil && oauthMetadataActive(item.Expiry) {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].InstallationKey != filtered[j].InstallationKey {
			return filtered[i].InstallationKey < filtered[j].InstallationKey
		}
		if filtered[i].TokenType != filtered[j].TokenType {
			return filtered[i].TokenType < filtered[j].TokenType
		}
		return filtered[i].UpdatedAt.Before(filtered[j].UpdatedAt)
	})
	return filtered[0], nil
}

func oauthMetadataActive(expiry time.Time) bool {
	return expiry.IsZero() || expiry.After(time.Now().UTC())
}

func verifiedDelegatedActorPlatformUserID(req BuildRequest) (string, bool) {
	userID := strings.TrimSpace(req.Actor.PlatformUserID)
	if !req.Actor.Verified || userID == "" {
		return "", false
	}
	if strings.HasPrefix(strings.TrimSpace(req.Actor.Principal), "system:") {
		return "", false
	}
	if strings.TrimSpace(req.CurrentSessionID) == "" || strings.TrimSpace(req.Actor.SessionID) != strings.TrimSpace(req.CurrentSessionID) {
		return "", false
	}
	if strings.TrimSpace(req.CurrentTurnContextID) == "" || strings.TrimSpace(req.Actor.TurnContextID) != strings.TrimSpace(req.CurrentTurnContextID) {
		return "", false
	}
	if strings.TrimSpace(req.CurrentRequestID) == "" || strings.TrimSpace(req.Actor.RequestID) != strings.TrimSpace(req.CurrentRequestID) {
		return "", false
	}
	if req.Actor.ExpiresAt.IsZero() || !req.Actor.ExpiresAt.After(time.Now().UTC()) {
		return "", false
	}
	return userID, true
}

func buildUserDelegatedContexts(store Store, req BuildRequest, projection *Projection) error {
	builders := []struct {
		path    string
		service string
		build   func(Store, BuildRequest, string) (string, error)
	}{
		{PathJiraContext, registry.OAuthServiceJira, buildDelegatedJiraContext},
		{PathNotionContext, registry.OAuthServiceNotion, buildDelegatedNotionContext},
		{PathGoogleWorkspacePolicy, registry.OAuthServiceGoogleWorkspace, buildDelegatedGoogleWorkspacePolicy},
		{PathTempoContext, registry.OAuthServiceTempo, buildDelegatedTempoContext},
		{PathGitLabContext, registry.OAuthServiceGitLab, buildDelegatedGitLabContext},
	}
	omitAll := func() {
		for _, builder := range builders {
			projection.OmittedPaths = append(projection.OmittedPaths, builder.path)
		}
	}
	userID, ok := verifiedDelegatedActorPlatformUserID(req)
	if !ok {
		omitAll()
		return nil
	}
	denied, err := store.IsUserAgentImpersonationDenied(userID, req.AgentID)
	if err != nil {
		return err
	}
	if denied {
		omitAll()
		return nil
	}
	for _, builder := range builders {
		if blocked, err := serviceBlockedByPlaceFilter(store, req.PlaceID, builder.service); err != nil {
			return err
		} else if blocked {
			projection.OmittedPaths = append(projection.OmittedPaths, builder.path)
			continue
		}
		content, err := builder.build(store, req, userID)
		if err != nil {
			return err
		}
		if content == "" {
			missing, err := userConnectionRequiredProjection(req, builder.service)
			if err != nil {
				return err
			}
			if missing == "" {
				projection.OmittedPaths = append(projection.OmittedPaths, builder.path)
				continue
			}
			projection.Files = append(projection.Files, File{Path: builder.path, Content: missing})
			continue
		}
		projection.Files = append(projection.Files, File{Path: builder.path, Content: content})
	}
	return nil
}

func userConnectionRequiredProjection(req BuildRequest, service string) (string, error) {
	connectURL := userOAuthConnectURL(req.PublicBaseURL, service)
	if connectURL == "" {
		return "", nil
	}
	return StableJSON(struct {
		Service    string            `json:"service"`
		Reason     string            `json:"reason"`
		ConnectURL string            `json:"connectUrl"`
		Details    map[string]string `json:"details"`
	}{
		Service:    service,
		Reason:     "user_connection_required",
		ConnectURL: connectURL,
		Details:    map[string]string{"service": service},
	})
}

func userOAuthConnectURL(publicBaseURL, service string) string {
	base := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	service = strings.TrimSpace(service)
	if base == "" || service == "" {
		return ""
	}
	values := url.Values{}
	values.Set("oauth_service", service)
	return base + "/#/settings/connections?" + values.Encode()
}

func serviceBlockedByPlaceFilter(store Store, placeID, service string) (bool, error) {
	if strings.TrimSpace(placeID) == "" {
		return false, nil
	}
	resource := registry.PlaceCredentialFilterResourceKey(service)
	if resource == "" {
		return false, nil
	}
	filters, hasFilters, err := store.PlaceCredentialFilterMap(placeID, service)
	if err != nil || !hasFilters {
		return false, err
	}
	level, ok := filters[resource]
	return ok && strings.EqualFold(strings.TrimSpace(level), registry.OAuthAccessNone), nil
}

func buildDelegatedJiraContext(store Store, req BuildRequest, userID string) (string, error) {
	connection, err := firstUserScopedMetadata(store, req, registry.OAuthServiceJira, userID)
	if err != nil || connection == nil {
		return "", err
	}
	metadata, err := oauthpkg.ParseJiraMetadata(connection.MetadataJSON)
	if err != nil {
		return "", fmt.Errorf("parse delegated jira metadata: %w", err)
	}
	if metadata.SelectedSite == nil {
		return "", nil
	}
	type selectedSite struct {
		CloudID string `json:"cloudId"`
	}
	return StableJSON(struct {
		Service      string       `json:"service"`
		Mode         string       `json:"mode"`
		SelectedSite selectedSite `json:"selectedSite"`
	}{
		Service: registry.OAuthServiceJira,
		Mode:    metadata.Mode,
		SelectedSite: selectedSite{
			CloudID: metadata.SelectedSite.CloudID,
		},
	})
}

func buildDelegatedNotionContext(store Store, req BuildRequest, userID string) (string, error) {
	connection, err := firstUserScopedMetadata(store, req, registry.OAuthServiceNotion, userID)
	if err != nil || connection == nil {
		return "", err
	}
	if _, err := oauthpkg.ParseNotionMetadata(connection.MetadataJSON); err != nil {
		return "", fmt.Errorf("parse delegated notion metadata: %w", err)
	}
	return StableJSON(struct {
		Service    string `json:"service"`
		APIVersion string `json:"apiVersion"`
	}{
		Service:    registry.OAuthServiceNotion,
		APIVersion: oauthpkg.NotionAPIVersion,
	})
}

func buildDelegatedGoogleWorkspacePolicy(store Store, req BuildRequest, userID string) (string, error) {
	connection, err := firstUserScopedMetadata(store, req, registry.OAuthServiceGoogleWorkspace, userID)
	if err != nil || connection == nil {
		return "", err
	}
	permissions, err := store.GetUserOAuthPermissionMap(userID, registry.OAuthServiceGoogleWorkspace)
	if err != nil {
		return "", err
	}
	permissions, err = narrowedGoogleWorkspacePermissions(store, req.PlaceID, permissions)
	if err != nil {
		return "", err
	}
	if googleWorkspacePermissionsAllNone(permissions) {
		return "", nil
	}
	return StableJSON(struct {
		Service      string            `json:"service"`
		Applications map[string]string `json:"applications"`
	}{
		Service:      registry.OAuthServiceGoogleWorkspace,
		Applications: permissions,
	})
}

func buildDelegatedTempoContext(store Store, req BuildRequest, userID string) (string, error) {
	connection, err := firstUserScopedMetadata(store, req, registry.OAuthServiceTempo, userID)
	if err != nil || connection == nil {
		return "", err
	}
	metadata, err := oauthpkg.ParseTempoMetadata(connection.MetadataJSON)
	if err != nil {
		return "", fmt.Errorf("parse delegated tempo metadata: %w", err)
	}
	return StableJSON(struct {
		Service   string `json:"service"`
		AccountID string `json:"accountId,omitempty"`
	}{
		Service:   registry.OAuthServiceTempo,
		AccountID: metadata.AccountID,
	})
}

func buildDelegatedGitLabContext(store Store, req BuildRequest, userID string) (string, error) {
	connection, err := firstUserScopedMetadata(store, req, registry.OAuthServiceGitLab, userID)
	if err != nil || connection == nil {
		return "", err
	}
	metadata, err := oauthpkg.ParseGitLabMetadata(connection.MetadataJSON)
	if err != nil {
		return "", fmt.Errorf("parse delegated gitlab metadata: %w", err)
	}
	return StableJSON(struct {
		Service string `json:"service"`
		Host    string `json:"host,omitempty"`
		BaseURL string `json:"baseUrl,omitempty"`
	}{
		Service: registry.OAuthServiceGitLab,
		Host:    metadata.Host,
		BaseURL: metadata.BaseURL,
	})
}

func buildJiraContext(store Store, req BuildRequest) (string, error) {
	connection, err := firstAgentScopedMetadata(store, req, registry.OAuthServiceJira)
	if err != nil || connection == nil {
		return "", err
	}
	metadata, err := oauthpkg.ParseJiraMetadata(connection.MetadataJSON)
	if err != nil {
		return "", fmt.Errorf("parse jira metadata: %w", err)
	}
	if metadata.SelectedSite == nil {
		return "", nil
	}
	type selectedSite struct {
		CloudID string `json:"cloudId"`
	}
	return StableJSON(struct {
		Service      string       `json:"service"`
		Mode         string       `json:"mode"`
		SelectedSite selectedSite `json:"selectedSite"`
	}{
		Service: registry.OAuthServiceJira,
		Mode:    metadata.Mode,
		SelectedSite: selectedSite{
			CloudID: metadata.SelectedSite.CloudID,
		},
	})
}

func buildNotionContext(store Store, req BuildRequest) (string, error) {
	connection, err := firstAgentScopedMetadata(store, req, registry.OAuthServiceNotion)
	if err != nil || connection == nil {
		return "", err
	}
	if _, err := oauthpkg.ParseNotionMetadata(connection.MetadataJSON); err != nil {
		return "", fmt.Errorf("parse notion metadata: %w", err)
	}
	return StableJSON(struct {
		Service    string `json:"service"`
		APIVersion string `json:"apiVersion"`
	}{
		Service:    registry.OAuthServiceNotion,
		APIVersion: oauthpkg.NotionAPIVersion,
	})
}

func buildGoogleWorkspacePolicy(store Store, req BuildRequest) (string, error) {
	connection, err := firstAgentScopedMetadata(store, req, registry.OAuthServiceGoogleWorkspace)
	if err != nil || connection == nil {
		return "", err
	}
	permissions, err := store.GetOAuthPermissionMapForCompartment(req.AgentID, req.CompartmentID, registry.OAuthServiceGoogleWorkspace)
	if err != nil {
		return "", err
	}
	permissions, err = narrowedGoogleWorkspacePermissions(store, req.PlaceID, permissions)
	if err != nil {
		return "", err
	}
	if googleWorkspacePermissionsAllNone(permissions) {
		return "", nil
	}
	return StableJSON(struct {
		Service      string            `json:"service"`
		Applications map[string]string `json:"applications"`
	}{
		Service:      registry.OAuthServiceGoogleWorkspace,
		Applications: permissions,
	})
}

func googleWorkspacePermissionsAllNone(permissions map[string]string) bool {
	for _, resource := range registry.GoogleWorkspaceResources {
		if strings.TrimSpace(permissions[resource]) != "" && strings.TrimSpace(permissions[resource]) != registry.OAuthAccessNone {
			return false
		}
	}
	return true
}

func narrowedGoogleWorkspacePermissions(store Store, placeID string, permissions map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(registry.GoogleWorkspaceResources))
	for _, resource := range registry.GoogleWorkspaceResources {
		level := strings.TrimSpace(permissions[resource])
		if level == "" {
			level = registry.OAuthAccessNone
		}
		out[resource] = level
	}
	if strings.TrimSpace(placeID) == "" {
		return out, nil
	}
	filters, hasFilters, err := store.PlaceCredentialFilterMap(placeID, registry.OAuthServiceGoogleWorkspace)
	if err != nil || !hasFilters {
		return out, err
	}
	for _, resource := range registry.GoogleWorkspaceResources {
		if level, ok := filters[resource]; ok {
			out[resource] = narrowAccess(out[resource], level)
		}
	}
	return out, nil
}

func narrowAccess(base, filter string) string {
	if accessRank(filter) < accessRank(base) {
		return strings.TrimSpace(filter)
	}
	return strings.TrimSpace(base)
}

func accessRank(level string) int {
	switch strings.TrimSpace(level) {
	case registry.OAuthAccessWrite:
		return 2
	case registry.OAuthAccessRead:
		return 1
	default:
		return 0
	}
}

func buildTempoContext(store Store, req BuildRequest) (string, error) {
	connection, err := firstAgentScopedMetadata(store, req, registry.OAuthServiceTempo)
	if err != nil || connection == nil {
		return "", err
	}
	metadata, err := oauthpkg.ParseTempoMetadata(connection.MetadataJSON)
	if err != nil {
		return "", fmt.Errorf("parse tempo metadata: %w", err)
	}
	return StableJSON(struct {
		Service   string `json:"service"`
		AccountID string `json:"accountId,omitempty"`
	}{
		Service:   registry.OAuthServiceTempo,
		AccountID: metadata.AccountID,
	})
}

func buildGitLabContext(store Store, req BuildRequest) (string, error) {
	connection, err := firstAgentScopedMetadata(store, req, registry.OAuthServiceGitLab)
	if err != nil || connection == nil {
		return "", err
	}
	metadata, err := oauthpkg.ParseGitLabMetadata(connection.MetadataJSON)
	if err != nil {
		return "", fmt.Errorf("parse gitlab metadata: %w", err)
	}
	return StableJSON(struct {
		Service string `json:"service"`
		Host    string `json:"host,omitempty"`
		BaseURL string `json:"baseUrl,omitempty"`
	}{
		Service: registry.OAuthServiceGitLab,
		Host:    metadata.Host,
		BaseURL: metadata.BaseURL,
	})
}

func buildGitHubContext(store Store, req BuildRequest) (string, error) {
	metadata, err := store.GetAgentScopedOAuthMetadataForCompartment(req.AgentID, req.CompartmentID, registry.OAuthServiceGitHub)
	if err != nil {
		return "", err
	}
	type contextInstallation struct {
		Login               string   `json:"login,omitempty"`
		RepositorySelection string   `json:"repositorySelection,omitempty"`
		AccessibleRepos     []string `json:"accessibleRepos,omitempty"`
		RepoListComplete    bool     `json:"repoListComplete,omitempty"`
	}
	type contextFile struct {
		Service           string                `json:"service"`
		Host              string                `json:"host,omitempty"`
		InstallationCount int                   `json:"installationCount"`
		RepositoryCount   int                   `json:"repositoryCount"`
		AccessibleRepos   []string              `json:"accessibleRepos,omitempty"`
		RepoListComplete  bool                  `json:"repoListComplete,omitempty"`
		Installations     []contextInstallation `json:"installations,omitempty"`
	}

	metadataList := make([]oauthpkg.GitHubMetadata, 0, len(metadata))
	repoSet := map[string]struct{}{}
	host := ""
	repositoryCount := 0
	repoListComplete := true
	for _, connection := range metadata {
		if connection == nil ||
			!oauthMetadataActive(connection.Expiry) ||
			!strings.EqualFold(strings.TrimSpace(connection.TokenType), "GitHubAppInstallation") {
			continue
		}
		metadata, err := oauthpkg.ParseGitHubMetadata(connection.MetadataJSON)
		if err != nil {
			return "", err
		}
		metadataList = append(metadataList, metadata)
		if host == "" {
			host = metadata.Host
		}
		repositoryCount += metadata.InstalledRepositories
		if !metadata.RepoListComplete {
			repoListComplete = false
		}
		for _, repo := range metadata.AccessibleRepos {
			repo = strings.TrimSpace(repo)
			if repo != "" {
				repoSet[repo] = struct{}{}
			}
		}
	}
	if len(metadataList) == 0 {
		return "", nil
	}
	sort.Slice(metadataList, func(i, j int) bool {
		if metadataList[i].Host != metadataList[j].Host {
			return metadataList[i].Host < metadataList[j].Host
		}
		if metadataList[i].AccountLogin != metadataList[j].AccountLogin {
			return metadataList[i].AccountLogin < metadataList[j].AccountLogin
		}
		return metadataList[i].InstallationID < metadataList[j].InstallationID
	})

	accessibleRepos := make([]string, 0, len(repoSet))
	for repo := range repoSet {
		accessibleRepos = append(accessibleRepos, repo)
	}
	sort.Strings(accessibleRepos)

	filtered := false
	if strings.TrimSpace(req.PlaceID) != "" {
		accessibleRepos, filtered, err = filterGitHubRepos(store, req.PlaceID, accessibleRepos)
		if err != nil {
			return "", err
		}
		if filtered {
			repositoryCount = len(accessibleRepos)
			repoListComplete = true
		}
	}

	allowedRepos := map[string]struct{}{}
	for _, repo := range accessibleRepos {
		allowedRepos[strings.ToLower(strings.TrimSpace(repo))] = struct{}{}
	}
	installations := make([]contextInstallation, 0, len(metadataList))
	for _, metadata := range metadataList {
		repos := make([]string, 0, len(metadata.AccessibleRepos))
		for _, repo := range metadata.AccessibleRepos {
			repo = strings.TrimSpace(repo)
			if repo == "" {
				continue
			}
			if filtered {
				if _, ok := allowedRepos[strings.ToLower(repo)]; !ok {
					continue
				}
			}
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		selection := metadata.RepositorySelection
		if filtered {
			if len(repos) == 0 {
				continue
			}
			selection = "selected"
		}
		installationRepoListComplete := metadata.RepoListComplete
		if filtered {
			installationRepoListComplete = true
		}
		installations = append(installations, contextInstallation{
			Login:               metadata.AccountLogin,
			RepositorySelection: selection,
			AccessibleRepos:     repos,
			RepoListComplete:    installationRepoListComplete,
		})
	}

	return StableJSON(contextFile{
		Service:           registry.OAuthServiceGitHub,
		Host:              host,
		InstallationCount: len(installations),
		RepositoryCount:   repositoryCount,
		AccessibleRepos:   accessibleRepos,
		RepoListComplete:  repoListComplete,
		Installations:     installations,
	})
}

func filterGitHubRepos(store Store, placeID string, repos []string) ([]string, bool, error) {
	if strings.TrimSpace(placeID) == "" || len(repos) == 0 {
		return repos, false, nil
	}
	filters, hasFilters, err := store.PlaceCredentialFilterMap(placeID, registry.OAuthServiceGitHub)
	if err != nil {
		return nil, false, err
	}
	if !hasFilters {
		return repos, false, nil
	}
	folded := make(map[string]string, len(filters))
	for resource, level := range filters {
		folded[strings.ToLower(strings.TrimSpace(resource))] = strings.TrimSpace(level)
	}
	serviceLevel, hasServiceLevel := folded[registry.PlaceCredentialFilterResourceKey(registry.OAuthServiceGitHub)]
	filtered := make([]string, 0, len(repos))
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		level, ok := folded[strings.ToLower(repo)]
		if !ok && hasServiceLevel {
			level, ok = serviceLevel, true
		}
		if ok && strings.EqualFold(level, registry.OAuthAccessNone) {
			continue
		}
		filtered = append(filtered, repo)
	}
	return filtered, true, nil
}
