package microvm

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbePrivilegedLauncherPerformsVersionedPing(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "launcher.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	requestSeen := make(chan privilegedLauncherRequest, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		var req privilegedLauncherRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			serverErr <- err
			return
		}
		requestSeen <- req
		serverErr <- json.NewEncoder(conn).Encode(privilegedLauncherResponse{
			OK:             true,
			Version:        expectedFirecrackerVersionString(),
			NetworkPosture: &NetworkPostureReport{Healthy: true},
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := ProbePrivilegedLauncher(ctx, socketPath); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("fake launcher: %v", err)
	}
	req := <-requestSeen
	if req.Op != "ping" || req.Version != privilegedLauncherProtocolVersion {
		t.Fatalf("probe request = %+v", req)
	}
}

func TestProbePrivilegedLauncherRejectsMissingNetworkPosture(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "launcher.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var req privilegedLauncherRequest
		_ = json.NewDecoder(conn).Decode(&req)
		_ = json.NewEncoder(conn).Encode(privilegedLauncherResponse{OK: true, Version: expectedFirecrackerVersionString()})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := ProbePrivilegedLauncher(ctx, socketPath); err == nil || !strings.Contains(err.Error(), "network posture") {
		t.Fatalf("probe error = %v, want missing network posture", err)
	}
}

func TestProbePrivilegedLauncherRejectsMissingSocket(t *testing.T) {
	if err := ProbePrivilegedLauncher(context.Background(), ""); err == nil {
		t.Fatal("expected missing socket error")
	}
}

func TestWaitForPrivilegedLauncherRetriesUntilSocketIsReady(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "launcher.sock")
	ready := make(chan struct{})
	go func() {
		time.Sleep(25 * time.Millisecond)
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			close(ready)
			return
		}
		defer listener.Close()
		close(ready)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req privilegedLauncherRequest
		_ = json.NewDecoder(conn).Decode(&req)
		_ = json.NewEncoder(conn).Encode(privilegedLauncherResponse{
			OK:             true,
			Version:        expectedFirecrackerVersionString(),
			NetworkPosture: &NetworkPostureReport{Healthy: true},
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := WaitForPrivilegedLauncher(ctx, socketPath, 5*time.Millisecond); err != nil {
		t.Fatalf("wait for launcher: %v", err)
	}
	<-ready
}

func TestWaitForPrivilegedLauncherStopsAtContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := WaitForPrivilegedLauncher(ctx, filepath.Join(t.TempDir(), "missing.sock"), 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("wait error = %v, want context deadline", err)
	}
}
