package firecracker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// The adapter grants the guest only what the receive window has room for and
// returns credit as the caller reads. A caller that lags behind the guest
// therefore holds the guest back; it is never disconnected for being slow.
func TestGuestPortAdapterWithholdsCreditWhileCallerLags(t *testing.T) {
	stream := &recordingPortCreditStream{}
	connection := newGuestPortConnection(stream, &guestv1.OperationBinding{}, func() {})
	data, err := readWithGuest(t, connection, stream, 8)
	if err != nil || len(data) != 8 {
		t.Fatalf("first Read = %d bytes, error = %v", len(data), err)
	}
	if stream.granted != firecrackerGuestPortReceiveWindowBytes {
		t.Fatalf("initial grant = %d, want the receive window %d", stream.granted, firecrackerGuestPortReceiveWindowBytes)
	}
	// Eight freed bytes are below one grant unit, so the guest keeps what it
	// had and spends all of it before the caller reads again.
	fillGuestPortWindow(t, connection)
	if held := heldGuestPortBytes(connection); held != firecrackerGuestPortReceiveWindowBytes-8 {
		t.Fatalf("held bytes = %d, want %d", held, firecrackerGuestPortReceiveWindowBytes-8)
	}
	if err := connection.enqueueReceived([]byte{1}); err == nil {
		t.Fatal("guest exceeded the receive window without a protocol error")
	}
	// Reads below one grant unit free room without re-granting it.
	granted := stream.granted
	for range 3 {
		if _, err := connection.Read(t.Context(), firecrackerGuestPortCreditGrantBytes/4); err != nil {
			t.Fatal(err)
		}
	}
	if stream.granted != granted {
		t.Fatalf("partial reads granted %d more bytes", stream.granted-granted)
	}
	if _, err := connection.Read(t.Context(), firecrackerGuestPortCreditGrantBytes/4); err != nil {
		t.Fatal(err)
	}
	if want := uint64(firecrackerGuestPortCreditGrantBytes + 8); stream.granted-granted != want {
		t.Fatalf("returned credit = %d, want %d", stream.granted-granted, want)
	}
	if held := heldGuestPortBytes(connection); held != firecrackerGuestPortReceiveWindowBytes {
		t.Fatalf("held bytes after top-up = %d, want %d", held, firecrackerGuestPortReceiveWindowBytes)
	}
}

func TestGuestPortAdapterDeliversQueuedBytesBeforeTerminalOutcome(t *testing.T) {
	stream := &recordingPortCreditStream{}
	connection := newGuestPortConnection(stream, &guestv1.OperationBinding{}, func() {})
	connection.receiveCredit = 6
	for _, chunk := range []string{"ab", "cdef"} {
		if err := connection.enqueueReceived([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	connection.finishReceive(io.EOF)
	var received []byte
	for {
		data, err := connection.Read(t.Context(), 3)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			break
		}
		received = append(received, data...)
	}
	if string(received) != "abcdef" {
		t.Fatalf("received %q before the terminal outcome", received)
	}
}

func readWithGuest(
	t *testing.T,
	connection *guestPortConnection,
	stream *recordingPortCreditStream,
	size int,
) ([]byte, error) {
	t.Helper()
	stream.onGrant = func(uint64) {
		stream.onGrant = nil
		if err := connection.enqueueReceived(make([]byte, size)); err != nil {
			t.Error(err)
		}
	}
	return connection.Read(t.Context(), size)
}

func fillGuestPortWindow(t *testing.T, connection *guestPortConnection) {
	t.Helper()
	connection.receiveMu.Lock()
	remaining := connection.receiveCredit
	connection.receiveMu.Unlock()
	for remaining > 0 {
		size := min(remaining, firecrackerGuestPortFrameBytes)
		if err := connection.enqueueReceived(make([]byte, size)); err != nil {
			t.Fatal(err)
		}
		remaining -= size
	}
}

func heldGuestPortBytes(connection *guestPortConnection) uint64 {
	connection.receiveMu.Lock()
	defer connection.receiveMu.Unlock()
	return connection.receiveCredit + connection.receivedBytes
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
		connection := newGuestPortConnection(stream, binding, func() {})
		connection.receiveCredit = 8
		connection.receive(t.Context())
		if err := <-connection.opened; err != nil {
			t.Fatalf("initial writable credit did not open the Port: %v", err)
		}
		if len(connection.received) != 0 || connection.receiveErr == nil ||
			!strings.Contains(connection.receiveErr.Error(), "bytes exceed") {
			t.Fatalf("uncredited frame size %d: queued %d frames, error = %v",
				size, len(connection.received), connection.receiveErr)
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

// A guest socket that produces faster than the caller consumes is slowed by
// withheld credit: the connection survives, the Runner holds at most one
// receive window, and every byte arrives in order. This runs the production
// guest Port pump behind the production adapter.
func TestGuestPortAdapterBackpressuresFastGuestProducer(t *testing.T) {
	socketPath, _, _ := startDirectUnixSocketGuest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	session := negotiateDirectUnixSocket(t, ctx, socketPath)
	defer session.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// Small paced writes make the guest pump emit many small frames, the shape
	// of an application streaming discrete messages; a plain bulk write would
	// coalesce into full frames.
	const total, chunkBytes = 2 << 20, 1 << 10
	producerErrors := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			producerErrors <- err
			return
		}
		defer connection.Close()
		chunk := make([]byte, chunkBytes)
		for offset := 0; offset < total; offset += len(chunk) {
			for index := range chunk {
				chunk[index] = byte((offset + index) % 251)
			}
			if _, err := connection.Write(chunk); err != nil {
				producerErrors <- err
				return
			}
			time.Sleep(20 * time.Microsecond)
		}
		producerErrors <- nil
	}()
	port, err := OpenPortOverSession(ctx, session, "assignment-1", &runnerprotocol.PortOpen{
		GuestPort: uint32(listener.Addr().(*net.TCPAddr).Port),
		Protocol:  "tcp", IdleTimeoutMs: 30_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	adapter := port.(*guestPortConnection)
	received := 0
	maximumHeld := uint64(0)
	for received < total {
		// The caller is slower than the producer throughout.
		time.Sleep(200 * time.Microsecond)
		maximumHeld = max(maximumHeld, heldGuestPortBytes(adapter))
		data, err := port.Read(ctx, firecrackerGuestPortFrameBytes)
		if err != nil {
			t.Fatalf("Port read after %d of %d bytes: %v", received, total, err)
		}
		for index, value := range data {
			if value != byte((received+index)%251) {
				t.Fatalf("byte %d = %d, want %d", received+index, value, (received+index)%251)
			}
		}
		received += len(data)
	}
	if maximumHeld > firecrackerGuestPortReceiveWindowBytes {
		t.Fatalf("Runner held %d guest bytes, window is %d", maximumHeld, firecrackerGuestPortReceiveWindowBytes)
	}
	select {
	case err := <-producerErrors:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
