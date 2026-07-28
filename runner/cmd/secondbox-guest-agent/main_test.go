package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWaitRuntimeEnvReadsDeliveredSecretEnv(t *testing.T) {
	privateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(privateDir, "env.json"), []byte(`{"SECONDBOX_SANDBOX_ID":"sandbox-1","SECONDBOX_RUNTIME_CREDENTIAL_ID":"opaque"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := waitRuntimeEnv(context.Background(), privateDir, time.Second)
	if err != nil {
		t.Fatalf("wait runtime env: %v", err)
	}
	if env["SECONDBOX_SANDBOX_ID"] != "sandbox-1" || env["SECONDBOX_RUNTIME_CREDENTIAL_ID"] != "opaque" {
		t.Fatalf("env = %#v", env)
	}
}

func TestVsockConnReadDeadlineUsesKernelSocketTimeout(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := &vsockConn{fd: fds[0]}
	defer conn.Close()
	defer unix.Close(fds[1])
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	started := time.Now()
	var value [1]byte
	_, err = conn.Read(value[:])
	if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("read deadline error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("socket deadline took %s", elapsed)
	}
}

func TestAppendEnvOverlaysExistingValues(t *testing.T) {
	got := appendEnv([]string{"PATH=/bin", "SECONDBOX_SANDBOX_ID=old", "NO_EQUALS"}, map[string]string{
		"SECONDBOX_SANDBOX_ID":            "sandbox-1",
		"SECONDBOX_RUNTIME_CREDENTIAL_ID": "opaque",
		"  ":                              "ignored",
	})
	env := map[string]string{}
	for _, item := range got {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			env[key] = value
		}
	}
	if env["PATH"] != "/bin" || env["SECONDBOX_SANDBOX_ID"] != "sandbox-1" || env["SECONDBOX_RUNTIME_CREDENTIAL_ID"] != "opaque" {
		t.Fatalf("merged env = %#v", env)
	}
	if _, ok := env["NO_EQUALS"]; ok {
		t.Fatalf("invalid env entry preserved: %#v", env)
	}
}
