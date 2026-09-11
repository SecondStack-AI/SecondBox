package egressattribution

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecutionAttributionAuthenticatesRunnerPeerBeforeReading(t *testing.T) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "peer.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	caller, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer caller.Close()
	accepted, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	// No bytes are sent: a wrong peer must fail without waiting for the preface deadline.
	if _, err := ReadRunnerExecutionAttribution(accepted, uint32(os.Getuid())+1, time.Now().Add(time.Second)); err == nil || !strings.Contains(err.Error(), "Runner peer denied") {
		t.Fatalf("wrong peer error = %v", err)
	}
	if _, err := ReadRunnerExecutionAttribution(accepted, uint32(os.Getuid()), time.Now().Add(10*time.Millisecond)); err == nil {
		t.Fatal("silent peer bypassed deadline")
	}
	if err := WriteExecutionAttribution(caller, testExecutionAttribution()); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRunnerExecutionAttribution(accepted, uint32(os.Getuid()), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("configured peer denied: %v", err)
	}
}
