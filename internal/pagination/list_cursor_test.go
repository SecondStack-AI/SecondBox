package pagination

import (
	"errors"
	"strings"
	"testing"
)

func TestListCursorIsCanonicalScopedAndOpaque(t *testing.T) {
	cursor, err := EncodeListCursor("sandboxes", "project-secret-identity", "sandbox-secret-identity")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cursor, "project-secret-identity") || strings.Contains(cursor, "sandbox-secret-identity") {
		t.Fatalf("list cursor exposes unencoded identity material: %q", cursor)
	}
	key, err := DecodeListCursor("sandboxes", "project-secret-identity", cursor)
	if err != nil {
		t.Fatal(err)
	}
	if key != "sandbox-secret-identity" {
		t.Fatalf("decoded list cursor key = %q", key)
	}
	for _, decode := range []struct {
		name         string
		resourceKind string
		scope        string
		cursor       string
	}{
		{name: "resource", resourceKind: "projects", scope: "project-secret-identity", cursor: cursor},
		{name: "scope", resourceKind: "sandboxes", scope: "other-project", cursor: cursor},
		{name: "malformed", resourceKind: "sandboxes", scope: "project-secret-identity", cursor: "not-a-cursor"},
		{
			name: "oversized", resourceKind: "sandboxes", scope: "project-secret-identity",
			cursor: strings.Repeat("a", MaximumListCursorLength+1),
		},
	} {
		t.Run(decode.name, func(t *testing.T) {
			if _, err := DecodeListCursor(decode.resourceKind, decode.scope, decode.cursor); !errors.Is(err, ErrInvalidListCursor) {
				t.Fatalf("DecodeListCursor error = %v, want ErrInvalidListCursor", err)
			}
		})
	}
}
