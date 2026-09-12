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

func TestGuestPortAdapterBoundsReadsAndGrantsOnlyAdditionalCredit(t *testing.T) {
	stream := &recordingPortCreditStream{}
	connection := &guestPortConnection{
		stream: stream, binding: &guestv1.OperationBinding{},
		reads: make(chan guestPortRead, 5), cancel: func() {},
	}
	for _, data := range []string{"a", "b", "cdefgh", "i", "jkl"} {
		connection.reads <- guestPortRead{data: []byte(data)}
	}
	for _, step := range []struct {
		maximum int
		want    string
		granted uint64
	}{
		{8, "a", 8}, {7, "b", 8}, {2, "cd", 8}, {2, "ef", 8},
		{2, "gh", 8}, {1, "i", 9}, {3, "jkl", 12},
	} {
		data, err := connection.Read(t.Context(), step.maximum)
		if err != nil || string(data) != step.want || stream.granted != step.granted {
			t.Fatalf("Read(%d) = %q, error = %v, total credit = %d; want %q, %d",
				step.maximum, data, err, stream.granted, step.want, step.granted)
		}
	}
}

func TestGuestPortAdapterRejectsBytesBeyondGrantedCredit(t *testing.T) {
	connection := &guestPortConnection{
		stream: &recordingPortCreditStream{}, binding: &guestv1.OperationBinding{},
		reads: make(chan guestPortRead, 1), cancel: func() {},
	}
	connection.reads <- guestPortRead{data: []byte("123456789")}
	data, err := connection.Read(t.Context(), 8)
	if len(data) != 0 || err == nil || !strings.Contains(err.Error(), "exceed granted credit") {
		t.Fatalf("over-credit Port read = %q, error = %v", data, err)
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
	guestv1.GuestAgent_ConnectClient
	granted uint64
}

func (stream *recordingPortCreditStream) Send(frame *guestv1.RunnerToGuest) error {
	stream.granted += frame.GetPort().GetCredit().GetByteCount()
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
