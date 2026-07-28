package microvmguest

import (
	"bytes"
	"io"
	"net"
	"testing"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

func TestGuestPortProxyUsesOnlyApprovedLoopbackDialAndByteCredit(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().(*net.TCPAddr)
	if !address.IP.IsLoopback() {
		t.Fatalf("test guest endpoint is not loopback: %s", address)
	}
	serverErrors := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.Close()
		request := make([]byte, 4)
		if _, err := io.ReadFull(connection, request); err != nil {
			serverErrors <- err
			return
		}
		if !bytes.Equal(request, []byte{0, 1, 0xfe, 2}) {
			serverErrors <- io.ErrUnexpectedEOF
			return
		}
		_, err = connection.Write([]byte{0xff, 3})
		serverErrors <- err
	}()

	stream, connectionBinding, cleanup := openNegotiatedProtocolTestStreamWithFeatures(
		t, t.TempDir(), []guestv1.GuestFeature{guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY},
	)
	defer cleanup()
	operationID := "port-loopback"
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Port{Port: &guestv1.PortFrame{
			Binding: protocolTestOperationBinding(connectionBinding, operationID, 1),
			Payload: &guestv1.PortFrame_Request{Request: &guestv1.PortRequest{
				GuestPort: uint32(address.Port), Protocol: "tcp", IdleTimeoutMs: 30_000,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	initial := receiveProtocolPort(t, stream)
	if initial.GetCredit() == nil || initial.GetCredit().ByteCount != guestPortFrameBytes {
		t.Fatalf("initial guest Port credit = %#v", initial.GetCredit())
	}
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Port{Port: &guestv1.PortFrame{
			Binding: protocolTestOperationBinding(connectionBinding, operationID, 2),
			Payload: &guestv1.PortFrame_Bytes{Bytes: &guestv1.PortBytes{
				Data: []byte{0, 1, 0xfe, 2},
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Port{Port: &guestv1.PortFrame{
			Binding: protocolTestOperationBinding(connectionBinding, operationID, 3),
			Payload: &guestv1.PortFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: 2}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	replenished := receiveProtocolPort(t, stream)
	if replenished.GetCredit() == nil || replenished.GetCredit().ByteCount != 4 {
		t.Fatalf("replenished guest Port credit = %#v", replenished.GetCredit())
	}
	response := receiveProtocolPort(t, stream)
	if response.GetBytes() == nil || !bytes.Equal(response.GetBytes().Data, []byte{0xff, 3}) {
		t.Fatalf("guest Port response = %#v", response.GetBytes())
	}
	if err := <-serverErrors; err != nil {
		t.Fatal(err)
	}
}

func TestGuestPortProxyRejectsUnapprovedRequestShape(t *testing.T) {
	stream, connectionBinding, cleanup := openNegotiatedProtocolTestStreamWithFeatures(
		t, t.TempDir(), []guestv1.GuestFeature{guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY},
	)
	defer cleanup()
	if err := stream.Send(&guestv1.RunnerToGuest{
		Message: &guestv1.RunnerToGuest_Port{Port: &guestv1.PortFrame{
			Binding: protocolTestOperationBinding(connectionBinding, "port-invalid", 1),
			Payload: &guestv1.PortFrame_Request{Request: &guestv1.PortRequest{
				GuestPort: 0, Protocol: "udp", IdleTimeoutMs: 30_000,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	terminal := receiveProtocolPort(t, stream).GetTerminal()
	if terminal == nil || terminal.Kind != guestv1.PortTerminalKind_PORT_TERMINAL_KIND_POLICY_DENIED {
		t.Fatalf("invalid guest Port terminal = %#v", terminal)
	}
}

func receiveProtocolPort(
	t *testing.T,
	stream guestv1.GuestAgent_ConnectClient,
) *guestv1.PortFrame {
	t.Helper()
	frame, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if frame.GetPort() == nil {
		t.Fatalf("frame = %#v, want Port", frame)
	}
	return frame.GetPort()
}
