package runtimecontext

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	PathGitHubContext             = "oauth/github.context.json"
	PathJiraContext               = "oauth/jira.context.json"
	PathNotionContext             = "oauth/notion.context.json"
	PathGoogleWorkspacePolicy     = "oauth/google-workspace.policy.json"
	PathTempoContext              = "oauth/tempo.context.json"
	PathGitLabContext             = "oauth/gitlab.context.json"
	PartialRolloutVersionGitHub   = "runtime-context-github-v1"
	PartialRolloutVersionServices = "runtime-context-services-v1"
)

var allowlistedPaths = map[string]struct{}{
	PathGitHubContext:         {},
	PathJiraContext:           {},
	PathNotionContext:         {},
	PathGoogleWorkspacePolicy: {},
	PathTempoContext:          {},
	PathGitLabContext:         {},
}

var allProjectionPaths = []string{
	PathGitHubContext,
	PathJiraContext,
	PathNotionContext,
	PathGoogleWorkspacePolicy,
	PathTempoContext,
	PathGitLabContext,
}

type VerifiedActorContext struct {
	Principal      string    `json:"principal,omitempty"`
	PlatformUserID string    `json:"platformUserId,omitempty"`
	SessionID      string    `json:"sessionId,omitempty"`
	Verified       bool      `json:"verified,omitempty"`
	Source         string    `json:"source,omitempty"`
	TurnContextID  string    `json:"turnContextId,omitempty"`
	RequestID      string    `json:"requestId,omitempty"`
	ExpiresAt      time.Time `json:"expiresAt,omitempty"`
}

type BuildRequest struct {
	AgentID              string               `json:"agentId,omitempty"`
	CompartmentID        string               `json:"compartmentId,omitempty"`
	PlaceID              string               `json:"placeId,omitempty"`
	PublicBaseURL        string               `json:"publicBaseUrl,omitempty"`
	EffectivePolicy      string               `json:"effectivePolicy,omitempty"`
	CurrentSessionID     string               `json:"currentSessionId,omitempty"`
	CurrentTurnContextID string               `json:"currentTurnContextId,omitempty"`
	CurrentRequestID     string               `json:"currentRequestId,omitempty"`
	Actor                VerifiedActorContext `json:"actor,omitempty"`
}

type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Projection struct {
	Files                 []File   `json:"files,omitempty"`
	OmittedPaths          []string `json:"omittedPaths,omitempty"`
	MigratedServicePaths  []string `json:"migratedServicePaths,omitempty"`
	EffectivePolicy       string   `json:"effectivePolicy,omitempty"`
	PartialRolloutVersion string   `json:"partialRolloutVersion,omitempty"`
}

func AllProjectionPaths() []string {
	return append([]string(nil), allProjectionPaths...)
}

func ValidatePath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("runtime context path is empty")
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return fmt.Errorf("runtime context path %q is absolute", path)
	}
	if strings.Contains(path, "\\") {
		return fmt.Errorf("runtime context path %q contains a backslash", path)
	}
	cleaned := filepath.ToSlash(filepath.Clean(path))
	if cleaned != path || cleaned == "." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return fmt.Errorf("runtime context path %q is not canonical", path)
	}
	if _, ok := allowlistedPaths[path]; !ok {
		return fmt.Errorf("runtime context path %q is not allowlisted", path)
	}
	return nil
}

func Canonicalize(projection Projection) (Projection, error) {
	out := Projection{
		EffectivePolicy:       strings.TrimSpace(projection.EffectivePolicy),
		PartialRolloutVersion: strings.TrimSpace(projection.PartialRolloutVersion),
	}
	seen := map[string]struct{}{}
	for _, file := range projection.Files {
		file.Path = strings.TrimSpace(file.Path)
		if err := ValidatePath(file.Path); err != nil {
			return Projection{}, err
		}
		if _, ok := seen[file.Path]; ok {
			return Projection{}, fmt.Errorf("duplicate runtime context path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
		out.Files = append(out.Files, file)
	}
	for _, path := range projection.OmittedPaths {
		path = strings.TrimSpace(path)
		if err := ValidatePath(path); err != nil {
			return Projection{}, err
		}
		out.OmittedPaths = append(out.OmittedPaths, path)
	}
	for _, path := range projection.MigratedServicePaths {
		path = strings.TrimSpace(path)
		if err := ValidatePath(path); err != nil {
			return Projection{}, err
		}
		out.MigratedServicePaths = append(out.MigratedServicePaths, path)
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	sort.Strings(out.OmittedPaths)
	sort.Strings(out.MigratedServicePaths)
	out.OmittedPaths = compactSortedStrings(out.OmittedPaths)
	out.MigratedServicePaths = compactSortedStrings(out.MigratedServicePaths)
	return out, nil
}

func (p Projection) IsZero() bool {
	return len(p.Files) == 0 &&
		len(p.OmittedPaths) == 0 &&
		len(p.MigratedServicePaths) == 0 &&
		strings.TrimSpace(p.EffectivePolicy) == "" &&
		strings.TrimSpace(p.PartialRolloutVersion) == ""
}

func (p Projection) FileContent(path string) (string, bool) {
	for _, file := range p.Files {
		if file.Path == path {
			return file.Content, true
		}
	}
	return "", false
}

func StableJSON(v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func compactSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := values[:0]
	var previous string
	for _, value := range values {
		if value == "" || value == previous {
			continue
		}
		out = append(out, value)
		previous = value
	}
	return out
}
