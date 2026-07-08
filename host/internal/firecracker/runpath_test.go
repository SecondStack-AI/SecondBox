package microvm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelocateRunDirForUnixSockets(t *testing.T) {
	// A short run dir leaves room for the per-instance socket suffix and must not move.
	t.Run("short path is left in place", func(t *testing.T) {
		got, ok := relocateRunDirForUnixSockets("/run/user/1000/agentcy")
		if ok || got != "" {
			t.Fatalf("expected no relocation, got (%q, %v)", got, ok)
		}
	})

	// The real-world failing path (deep DataDir) must relocate to something that
	// fits without using XDG_RUNTIME_DIR, which is too small for rootfs copies.
	t.Run("deep path relocates under cache dir", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "/tmp/agentcy-cache-test")
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		deep := "/home/sasha/Developer/tries/agent-manager/data/microvm/run"
		got, ok := relocateRunDirForUnixSockets(deep)
		if !ok {
			t.Fatalf("expected relocation for %q (len=%d)", deep, len(deep))
		}
		if strings.HasPrefix(got, "/run/user/1000/") {
			t.Fatalf("expected relocation outside XDG_RUNTIME_DIR, got %q", got)
		}
		want := "/tmp/agentcy-cache-test/agentcy/microvm/run"
		if got != want {
			t.Fatalf("expected cache relocation %q, got %q", want, got)
		}
		if len(got)+reservedRunDirBudget >= maxUnixSocketPathLen {
			t.Fatalf("relocated run dir %q (len=%d) still does not fit the socket budget", got, len(got))
		}
	})

	// With no usable cache dir, it falls back to a uid-scoped temp dir.
	t.Run("deep path falls back to temp dir", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("XDG_CACHE_HOME", "")
		t.Setenv("XDG_RUNTIME_DIR", "")
		deep := "/home/sasha/Developer/tries/agent-manager/data/microvm/run"
		got, ok := relocateRunDirForUnixSockets(deep)
		if !ok {
			t.Fatalf("expected relocation for %q", deep)
		}
		want := filepath.Join(os.TempDir(), fmt.Sprintf("agentcy-%d", os.Getuid()), "run")
		if got != want {
			t.Fatalf("expected fallback %q, got %q", want, got)
		}
	})
}

func TestCheckUnixSocketPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "fits", path: "/run/user/1000/agentcy/run/fc-abc-cmp-123-4567/firecracker.sock", wantErr: false},
		{name: "too long", path: "/" + strings.Repeat("a", maxUnixSocketPathLen), wantErr: true},
		{name: "exactly at limit is rejected", path: strings.Repeat("b", maxUnixSocketPathLen), wantErr: true},
		{name: "one under limit fits", path: strings.Repeat("c", maxUnixSocketPathLen-1), wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkUnixSocketPath("test", tt.path)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error for path of len %d", len(tt.path))
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error for path of len %d: %v", len(tt.path), err)
			}
		})
	}
}
