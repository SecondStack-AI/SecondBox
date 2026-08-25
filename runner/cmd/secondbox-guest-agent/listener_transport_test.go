package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectListenerTransportRequiresExactlyOneFamily(t *testing.T) {
	cases := []struct {
		name         string
		controlPort  int
		protocolPort int
		controlUnix  string
		protocolUnix string
		expectError  string
		expectUnix   bool
	}{
		{name: "vsock family", controlPort: 1024, protocolPort: 1025},
		{name: "unix family", controlUnix: "/run/a.sock", protocolUnix: "/run/b.sock", expectUnix: true},
		{name: "absent selection", expectError: "require vsock ports or Unix socket paths"},
		{name: "mixed families", controlPort: 1024, protocolPort: 1025, controlUnix: "/run/a.sock",
			protocolUnix: "/run/b.sock", expectError: "mutually exclusive"},
		{name: "partial vsock", controlPort: 1024, expectError: "distinct values"},
		{name: "identical vsock ports", controlPort: 1024, protocolPort: 1024, expectError: "distinct values"},
		{name: "partial unix", controlUnix: "/run/a.sock", expectError: "distinct paths"},
		{name: "identical unix paths", controlUnix: "/run/a.sock", protocolUnix: "/run/a.sock",
			expectError: "distinct paths"},
		{name: "unbounded unix path", controlUnix: "/" + strings.Repeat("x", 120), protocolUnix: "/run/b.sock",
			expectError: "exceeds"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport, err := selectListenerTransport(
				testCase.controlPort, testCase.protocolPort,
				testCase.controlUnix, testCase.protocolUnix,
			)
			if testCase.expectError != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.expectError) {
					t.Fatalf("error = %v, want %q", err, testCase.expectError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (transport.controlUnixSocket != "") != testCase.expectUnix {
				t.Fatalf("selected transport = %+v", transport)
			}
		})
	}
}

func TestListenUnixSocketReplacesStaleSocketAndRestrictsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guest.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// Closing removes the file for Go listeners; recreate a stale file to
	// prove replacement.
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := listenUnixSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", info.Mode().Perm())
	}
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial listening socket: %v", err)
	}
	_ = connection.Close()
}
