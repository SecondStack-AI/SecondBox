package microsandboxprotocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

type oneByteWriter struct{ bytes.Buffer }

func (writer *oneByteWriter) Write(value []byte) (int, error) {
	if len(value) > 1 {
		value = value[:1]
	}
	return writer.Buffer.Write(value)
}

func TestFrameRoundTripAndBounds(t *testing.T) {
	want := &Envelope{ProtocolVersion: Version, RequestId: 1, Message: &Envelope_Shutdown{Shutdown: &ShutdownRequest{}}}
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buffer)
	if err != nil || got.GetShutdown() == nil || got.RequestId != want.RequestId {
		t.Fatalf("round trip = %#v, %v", got, err)
	}
	var oversized [4]byte
	binary.BigEndian.PutUint32(oversized[:], MaxFrameBytes+1)
	if _, err := ReadFrame(bytes.NewReader(oversized[:])); !errors.Is(err, ErrFrameOversized) {
		t.Fatalf("oversized frame error = %v", err)
	}
}

func TestWriteFrameCompletesShortWrites(t *testing.T) {
	want := &Envelope{ProtocolVersion: Version, RequestId: 7, Message: &Envelope_Shutdown{Shutdown: &ShutdownRequest{}}}
	var writer oneByteWriter
	if err := WriteFrame(&writer, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(bytes.NewReader(writer.Bytes()))
	if err != nil || got.RequestId != want.RequestId {
		t.Fatalf("short-write round trip = %#v, %v", got, err)
	}
}

func FuzzReadFrameNeverPanics(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0})
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = ReadFrame(bytes.NewReader(payload))
	})
}
