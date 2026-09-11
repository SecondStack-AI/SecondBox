package egressforwarder

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
)

func forwarderTestAttribution() egressattribution.ExecutionAttribution {
	return egressattribution.ExecutionAttribution{
		TenantRef: "tenant", SubjectRef: "subject", SandboxID: "sandbox", InstanceID: "instance",
		AssignmentID: "assignment", Generation: 3, AuthorizationRef: "command",
		ExpiresAt: time.Now().UTC().Add(10 * time.Second).Truncate(time.Millisecond),
	}
}

func startTestForwarder(t *testing.T, socket string, attribution egressattribution.ExecutionAttribution, capacity int) (*net.TCPAddr, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- ForwardAttributedExecution(ctx, listener, socket, attribution, capacity) }()
	return listener.Addr().(*net.TCPAddr), cancel, done
}

func testForwarderGateway(t *testing.T) *net.UnixListener {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "gateway.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return listener
}

func dialTestForwarder(t *testing.T, address *net.TCPAddr) *net.TCPConn {
	t.Helper()
	connection, err := net.DialTCP("tcp", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return connection
}

func acceptForwarderAttribution(t *testing.T, listener *net.UnixListener, expected egressattribution.ExecutionAttribution) *net.UnixConn {
	t.Helper()
	connection, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	actual, err := egressattribution.ReadExecutionAttribution(connection, time.Now())
	if err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("attribution = %+v, error = %v; want %+v", actual, err, expected)
	}
	return connection
}

func waitTestForwarder(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("execution forwarder did not terminate")
		return nil
	}
}

func TestExecutionForwarderPreservesIdentityGuestBytesAndHalfClose(t *testing.T) {
	gateway := testForwarderGateway(t)
	expected := forwarderTestAttribution()
	address, cancel, done := startTestForwarder(t, gateway.Addr().String(), expected, 2)
	guest := dialTestForwarder(t, address)
	upstream := acceptForwarderAttribution(t, gateway, expected)
	request := "CONNECT api.example.com:443 HTTP/1.1\r\nX-Execution-Identity: forged\r\n\r\nSBXATTR1fake"
	if _, err := io.WriteString(guest, request); err != nil {
		t.Fatal(err)
	}
	if err := guest.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(upstream)
	if err != nil || string(actual) != request {
		t.Fatalf("guest bytes = %q, error = %v", actual, err)
	}
	if _, err := io.WriteString(upstream, "response after request EOF"); err != nil {
		t.Fatal(err)
	}
	if err := upstream.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	actual, err = io.ReadAll(guest)
	if err != nil || string(actual) != "response after request EOF" {
		t.Fatalf("response = %q, error = %v", actual, err)
	}
	cancel()
	if err := waitTestForwarder(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("termination = %v", err)
	}
}

func TestExecutionForwarderClosesActiveRelaysAndListener(t *testing.T) {
	for _, reason := range []string{"cancel", "expiry"} {
		t.Run(reason, func(t *testing.T) {
			gateway := testForwarderGateway(t)
			attribution := forwarderTestAttribution()
			if reason == "expiry" {
				attribution.ExpiresAt = time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
			}
			address, cancel, done := startTestForwarder(t, gateway.Addr().String(), attribution, 1)
			guest := dialTestForwarder(t, address)
			upstream := acceptForwarderAttribution(t, gateway, attribution)
			if reason == "cancel" {
				cancel()
			}
			if err := waitTestForwarder(t, done); err == nil {
				t.Fatal("termination did not report its cause")
			}
			for _, connection := range []net.Conn{guest, upstream} {
				if n, err := connection.Read(make([]byte, 1)); n != 0 || err != io.EOF {
					t.Fatalf("relay remained open: bytes = %d, error = %v", n, err)
				}
			}
			connection, err := net.DialTimeout("tcp", address.String(), time.Second)
			if err == nil {
				connection.Close()
				t.Fatal("listener accepted after termination")
			}
		})
	}
}

func TestExecutionForwarderRefusesCapacityWithoutDisturbingActiveRelay(t *testing.T) {
	gateway := testForwarderGateway(t)
	attribution := forwarderTestAttribution()
	address, cancel, done := startTestForwarder(t, gateway.Addr().String(), attribution, 1)
	first := dialTestForwarder(t, address)
	upstream := acceptForwarderAttribution(t, gateway, attribution)
	second := dialTestForwarder(t, address)
	if _, err := second.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("capacity refusal = %v", err)
	}
	if _, err := first.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if _, err := io.ReadFull(upstream, buffer); err != nil || string(buffer) != "x" {
		t.Fatalf("active relay = %q, error = %v", buffer, err)
	}
	cancel()
	if err := waitTestForwarder(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled forwarder = %v", err)
	}
}

func TestExecutionForwarderMissingGatewayFailsClosed(t *testing.T) {
	address, _, done := startTestForwarder(t, filepath.Join(t.TempDir(), "missing.sock"), forwarderTestAttribution(), 1)
	guest := dialTestForwarder(t, address)
	if err := waitTestForwarder(t, done); err == nil || !strings.Contains(err.Error(), "gateway connection failed") {
		t.Fatalf("missing gateway = %v", err)
	}
	if _, err := guest.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("guest was not closed: %v", err)
	}
}

func TestExecutionForwarderConnectionCloseKeepsGenerationUsable(t *testing.T) {
	for _, closeKind := range []string{"guest reset", "completed gateway response"} {
		t.Run(closeKind, func(t *testing.T) {
			gateway := testForwarderGateway(t)
			attribution := forwarderTestAttribution()
			address, cancel, done := startTestForwarder(t, gateway.Addr().String(), attribution, 2)
			guest := dialTestForwarder(t, address)
			upstream := acceptForwarderAttribution(t, gateway, attribution)
			if closeKind == "guest reset" {
				if err := guest.SetLinger(0); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := io.WriteString(upstream, "complete response"); err != nil {
					t.Fatal(err)
				}
				if err := upstream.Close(); err != nil {
					t.Fatal(err)
				}
				response, err := io.ReadAll(guest)
				if err != nil || string(response) != "complete response" {
					t.Fatalf("response = %q, error = %v", response, err)
				}
			}
			if err := guest.Close(); err != nil {
				t.Fatal(err)
			}
			if closeKind == "guest reset" {
				if _, err := upstream.Read(make([]byte, 1)); err != io.EOF {
					t.Fatalf("reset guest left its relay open: %v", err)
				}
			}
			select {
			case err := <-done:
				t.Fatalf("connection close terminated the generation: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			next := dialTestForwarder(t, address)
			nextUpstream := acceptForwarderAttribution(t, gateway, attribution)
			if _, err := io.WriteString(next, "next request"); err != nil {
				t.Fatal(err)
			}
			buffer := make([]byte, len("next request"))
			if _, err := io.ReadFull(nextUpstream, buffer); err != nil || string(buffer) != "next request" {
				t.Fatalf("next request = %q, error = %v", buffer, err)
			}
			cancel()
			if err := waitTestForwarder(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("termination = %v", err)
			}
		})
	}
}

func TestExecutionForwarderGatewayLossClosesExistingRelays(t *testing.T) {
	gateway := testForwarderGateway(t)
	attribution := forwarderTestAttribution()
	address, _, done := startTestForwarder(t, gateway.Addr().String(), attribution, 2)
	guest := dialTestForwarder(t, address)
	upstream := acceptForwarderAttribution(t, gateway, attribution)
	if err := gateway.Close(); err != nil {
		t.Fatal(err)
	}
	next := dialTestForwarder(t, address)
	if err := waitTestForwarder(t, done); err == nil || !strings.Contains(err.Error(), "gateway connection failed") {
		t.Fatalf("gateway loss = %v", err)
	}
	for _, connection := range []net.Conn{guest, upstream, next} {
		if _, err := connection.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("gateway loss left relay open: %v", err)
		}
	}
}
