// Package microsandboxprotocol implements the bounded private Runner/helper wire.
package microsandboxprotocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"google.golang.org/protobuf/proto"
)

const (
	Version       uint32 = 1
	MaxFrameBytes        = 1024 * 1024
)

var (
	ErrFrameOversized = errors.New("SecondBox Microsandbox helper frame exceeds bound")
	ErrFrameMalformed = errors.New("SecondBox Microsandbox helper frame is malformed")
	ErrProtocolState  = errors.New("SecondBox Microsandbox helper protocol state is invalid")
)

func ReadFrame(reader io.Reader) (*Envelope, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, fmt.Errorf("SecondBox Microsandbox helper frame header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 {
		return nil, ErrFrameMalformed
	}
	if length > MaxFrameBytes {
		return nil, ErrFrameOversized
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, fmt.Errorf("SecondBox Microsandbox helper frame payload: %w", err)
	}
	envelope := &Envelope{}
	if err := proto.Unmarshal(payload, envelope); err != nil || envelope.Message == nil {
		return nil, fmt.Errorf("%w: %v", ErrFrameMalformed, err)
	}
	return envelope, nil
}

func WriteFrame(writer io.Writer, envelope *Envelope) error {
	if envelope == nil || envelope.Message == nil {
		return ErrFrameMalformed
	}
	payload, err := proto.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("SecondBox Microsandbox helper frame encode: %w", err)
	}
	if len(payload) == 0 || len(payload) > MaxFrameBytes {
		return ErrFrameOversized
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeFull(writer, header[:]); err != nil {
		return fmt.Errorf("SecondBox Microsandbox helper frame header write: %w", err)
	}
	if err := writeFull(writer, payload); err != nil {
		return fmt.Errorf("SecondBox Microsandbox helper frame payload write: %w", err)
	}
	return nil
}

func writeFull(writer io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

type streamState struct {
	nextSequence uint64
	credit       uint64
	eof          bool
}

// State rejects duplicate requests and stale, uncredited, or post-EOF streams.
type State struct {
	mu       sync.Mutex
	requests map[uint64]struct{}
	streams  map[uint64]streamState
}

func NewState() *State {
	return &State{requests: map[uint64]struct{}{}, streams: map[uint64]streamState{}}
}

func (state *State) Admit(envelope *Envelope) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if envelope == nil || envelope.ProtocolVersion != Version || envelope.RequestId == 0 || envelope.Message == nil {
		return ErrProtocolState
	}
	if envelope.StreamId == 0 {
		if envelope.Sequence != 0 {
			return ErrProtocolState
		}
		if _, exists := state.requests[envelope.RequestId]; exists {
			return ErrProtocolState
		}
		state.requests[envelope.RequestId] = struct{}{}
		return nil
	}
	if _, exists := state.requests[envelope.RequestId]; !exists {
		return ErrProtocolState
	}
	stream := state.streams[envelope.StreamId]
	if stream.eof || envelope.Sequence != stream.nextSequence {
		return ErrProtocolState
	}
	switch message := envelope.Message.(type) {
	case *Envelope_StreamCredit:
		if message.StreamCredit.Bytes > ^uint64(0)-stream.credit {
			return ErrProtocolState
		}
		stream.credit += message.StreamCredit.Bytes
	case *Envelope_StreamData:
		if uint64(len(message.StreamData.Data)) > stream.credit {
			return ErrProtocolState
		}
		stream.credit -= uint64(len(message.StreamData.Data))
		stream.eof = message.StreamData.Eof
	default:
		return ErrProtocolState
	}
	stream.nextSequence++
	state.streams[envelope.StreamId] = stream
	return nil
}
