package scheduler

import (
	"strings"
	"testing"
)

func TestNewPostgresStoreRequiresClock(t *testing.T) {
	_, err := NewPostgresStore(t.Context(), PostgresStoreConfig{
		DatabaseURL: "postgres://unused",
	})
	if err == nil || !strings.Contains(err.Error(), "clock are required") {
		t.Fatalf("NewPostgresStore error = %v, want required clock", err)
	}
}
