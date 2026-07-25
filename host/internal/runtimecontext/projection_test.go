package runtimecontext

import "testing"

func TestCanonicalizeRejectsDuplicateProjectedPaths(t *testing.T) {
	_, err := Canonicalize(Projection{Files: []File{
		{Path: PathGitHubContext, Content: "{}"},
		{Path: PathGitHubContext, Content: "{\"other\":true}"},
	}})
	if err == nil {
		t.Fatal("expected duplicate path error")
	}
}

func TestCanonicalizeRejectsJiraTempoDuplicateContextOwner(t *testing.T) {
	_, err := Canonicalize(Projection{Files: []File{
		{Path: PathJiraContext, Content: `{"service":"jira"}`},
		{Path: PathJiraContext, Content: `{"service":"tempo"}`},
	}})
	if err == nil {
		t.Fatal("expected duplicate Jira context owner error")
	}
}

func TestCanonicalizeRejectsInvalidWriteAndDeletionPaths(t *testing.T) {
	cases := []Projection{
		{Files: []File{{Path: "../oauth/github.context.json", Content: "{}"}}},
		{Files: []File{{Path: "/workspace/config/oauth/github.context.json", Content: "{}"}}},
		{OmittedPaths: []string{"oauth/unknown.context.json"}},
		{MigratedServicePaths: []string{"oauth/../secrets.json"}},
	}
	for _, tc := range cases {
		if _, err := Canonicalize(tc); err == nil {
			t.Fatalf("Canonicalize(%#v) succeeded, want error", tc)
		}
	}
}

func TestCanonicalizeSortsProjectionState(t *testing.T) {
	got, err := Canonicalize(Projection{
		Files: []File{
			{Path: PathNotionContext, Content: "notion"},
			{Path: PathGitHubContext, Content: "github"},
		},
		OmittedPaths:         []string{PathGitLabContext, PathJiraContext, PathJiraContext},
		MigratedServicePaths: []string{PathNotionContext, PathGitHubContext},
	})
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if got.Files[0].Path != PathGitHubContext || got.Files[1].Path != PathNotionContext {
		t.Fatalf("files not sorted: %#v", got.Files)
	}
	if len(got.OmittedPaths) != 2 || got.OmittedPaths[0] != PathGitLabContext || got.OmittedPaths[1] != PathJiraContext {
		t.Fatalf("omitted paths not sorted/compacted: %#v", got.OmittedPaths)
	}
	if got.MigratedServicePaths[0] != PathGitHubContext || got.MigratedServicePaths[1] != PathNotionContext {
		t.Fatalf("migrated paths not sorted: %#v", got.MigratedServicePaths)
	}
}
