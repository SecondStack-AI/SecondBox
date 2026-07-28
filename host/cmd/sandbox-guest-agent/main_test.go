package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitRuntimeEnvReadsDeliveredSecretEnv(t *testing.T) {
	privateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(privateDir, "env.json"), []byte(`{"AGENT_ID":"agent-1","AGENT_PLATFORM_TOKEN":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := waitRuntimeEnv(context.Background(), privateDir, time.Second)
	if err != nil {
		t.Fatalf("wait runtime env: %v", err)
	}
	if env["AGENT_ID"] != "agent-1" || env["AGENT_PLATFORM_TOKEN"] != "secret" {
		t.Fatalf("env = %#v", env)
	}
}

func TestAppendEnvOverlaysExistingValues(t *testing.T) {
	got := appendEnv([]string{"PATH=/bin", "AGENT_ID=old", "NO_EQUALS"}, map[string]string{
		"AGENT_ID":             "agent-1",
		"AGENT_PLATFORM_TOKEN": "secret",
		"  ":                   "ignored",
	})
	env := map[string]string{}
	for _, item := range got {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			env[key] = value
		}
	}
	if env["PATH"] != "/bin" || env["AGENT_ID"] != "agent-1" || env["AGENT_PLATFORM_TOKEN"] != "secret" {
		t.Fatalf("merged env = %#v", env)
	}
	if _, ok := env["NO_EQUALS"]; ok {
		t.Fatalf("invalid env entry preserved: %#v", env)
	}
}
