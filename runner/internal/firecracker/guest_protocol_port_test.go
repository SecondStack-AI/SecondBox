package firecracker

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

func TestGuestPortAdapterBoundsReadsAndGrantsPerFrame(t *testing.T) {
	stream := &recordingPortCreditStream{}
	connection := &guestPortConnection{
		stream: stream, binding: &guestv1.OperationBinding{},
		reads: make(chan guestPortRead, 5), cancel: func() {},
	}
	for _, data := range []string{"a", "b", "cdefgh", "i", "jkl"} {
		connection.reads <- guestPortRead{data: []byte(data)}
	}
	// Every new frame is preceded by a grant of the caller's full bound; reads
	// served from a buffered frame grant nothing.
	for _, step := range []struct {
		maximum int
		want    string
		granted uint64
	}{
		{8, "a", 8}, {7, "b", 15}, {2, "cd", 17}, {2, "ef", 17},
		{2, "gh", 17}, {1, "i", 18}, {3, "jkl", 21},
	} {
		data, err := connection.Read(t.Context(), step.maximum)
		if err != nil || string(data) != step.want || stream.granted != step.granted {
			t.Fatalf("Read(%d) = %q, error = %v, total credit = %d; want %q, %d",
				step.maximum, data, err, stream.granted, step.want, step.granted)
		}
	}
}

// A guest agent from a signed bundle built before the short-read credit fix
// discards whatever part of a grant a short read left unused. The adapter must
// keep granting for each new frame instead of waiting on credit the guest no
// longer holds; otherwise every interactive Port stalls after its first short
// reply. The fake guest below models that legacy behavior exactly.
func TestGuestPortAdapterKeepsGrantingToLegacyGuestAfterShortReads(t *testing.T) {
	stream := &recordingPortCreditStream{}
	connection := &guestPortConnection{
		stream: stream, binding: &guestv1.OperationBinding{},
		reads: make(chan guestPortRead, 5), cancel: func() {},
	}
	legacyGuestCredit := uint64(0)
	stream.onGrant = func(granted uint64) {
		legacyGuestCredit += granted
		// The legacy guest reserves the whole grant for one socket read and
		// forgets the remainder after a short read; only the bytes read are
		// delivered.
		connection.reads <- guestPortRead{data: []byte("x")}
		legacyGuestCredit = 0
	}
	for i := range 3 {
		data, err := connection.Read(t.Context(), 8)
		if err != nil || string(data) != "x" {
			t.Fatalf("legacy short read %d = %q, error = %v", i, data, err)
		}
		if stream.granted != uint64(8*(i+1)) {
			t.Fatalf("legacy short read %d granted %d in total, want %d", i, stream.granted, 8*(i+1))
		}
	}
}

func TestGuestPortAdapterRejectsUncreditedFramesBeforeQueueing(t *testing.T) {
	for _, size := range []int{9, firecrackerGuestPortFrameBytes + 1} {
		binding := &guestv1.OperationBinding{Connection: &guestv1.ConnectionBinding{}, Sequence: 1}
		byteBinding := cloneGuestOperationBinding(binding)
		byteBinding.Sequence = 2
		stream := &scriptedPortReceiveStream{frames: []*guestv1.GuestToRunner{
			{Message: &guestv1.GuestToRunner_Port{Port: &guestv1.PortFrame{
				Binding: binding, Payload: &guestv1.PortFrame_Credit{Credit: &guestv1.ByteCredit{ByteCount: 8}},
			}}},
			{Message: &guestv1.GuestToRunner_Port{Port: &guestv1.PortFrame{
				Binding: byteBinding, Payload: &guestv1.PortFrame_Bytes{Bytes: &guestv1.PortBytes{Data: make([]byte, size)}},
			}}},
		}}
		connection := &guestPortConnection{
			stream: stream, binding: binding, receiveCredit: 8,
			credit: newGuestPortCredit(), reads: make(chan guestPortRead, 3), cancel: func() {},
		}
		connection.receive(t.Context())
		<-connection.reads // Initial writable credit acknowledges the Port opening.
		read := <-connection.reads
		if len(read.data) != 0 || read.err == nil || !strings.Contains(read.err.Error(), "bytes exceed") {
			t.Fatalf("uncredited frame size %d: queued data = %d bytes, error = %v", size, len(read.data), read.err)
		}
	}
}

type scriptedPortReceiveStream struct {
	guestv1.GuestAgent_ConnectClient
	frames []*guestv1.GuestToRunner
}

func (stream *scriptedPortReceiveStream) Recv() (*guestv1.GuestToRunner, error) {
	if len(stream.frames) == 0 {
		return nil, io.EOF
	}
	frame := stream.frames[0]
	stream.frames = stream.frames[1:]
	return frame, nil
}

type recordingPortCreditStream struct {
	onGrant func(uint64)
	guestv1.GuestAgent_ConnectClient
	granted uint64
}

func (stream *recordingPortCreditStream) Send(frame *guestv1.RunnerToGuest) error {
	granted := frame.GetPort().GetCredit().GetByteCount()
	stream.granted += granted
	if stream.onGrant != nil {
		stream.onGrant(granted)
	}
	return nil
}

func TestGuestPortAdapterPreservesCreditAcrossShortReadsAndBurst(t *testing.T) {
	socketPath, _, _ := startDirectUnixSocketGuest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	session := negotiateDirectUnixSocket(t, ctx, socketPath)
	defer session.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	echoErrors := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			echoErrors <- err
			return
		}
		defer connection.Close()
		_, err = io.Copy(connection, connection)
		echoErrors <- err
	}()
	port, err := OpenPortOverSession(ctx, session, "assignment-1", &runnerprotocol.PortOpen{
		GuestPort: uint32(listener.Addr().(*net.TCPAddr).Port),
		Protocol:  "tcp", IdleTimeoutMs: 5_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	// Use the production adapter and guest socket pump, with one eight-byte window.
	remaining := 8
	for _, payload := range [][]byte{{'a'}, {'b'}, []byte("cdefghi")} {
		if err := port.Write(ctx, payload); err != nil {
			t.Fatal(err)
		}
		var received []byte
		for len(received) < len(payload) && remaining > 0 {
			data, err := port.Read(ctx, remaining)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) == 0 || len(data) > remaining {
				t.Fatalf("Port read returned %d bytes against %d remaining credit", len(data), remaining)
			}
			remaining -= len(data)
			received = append(received, data...)
		}
		if !bytes.Equal(received, payload[:len(received)]) {
			t.Fatalf("Port response = %q, want prefix of %q", received, payload)
		}
	}
	// The ninth byte needs new caller capacity and must survive without loss.
	data, err := port.Read(ctx, 1)
	if err != nil || string(data) != "i" {
		t.Fatalf("Port response after replenishment = %q, error = %v", data, err)
	}
	if err := port.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-echoErrors:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
