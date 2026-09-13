// Package microsandboxprotocol implements the bounded private Runner/helper wire.
package microsandboxprotocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

const (
	Version       uint32 = 1
	MaxFrameBytes        = 1024 * 1024
)

var (
	ErrFrameOversized = errors.New("SecondBox Microsandbox helper frame exceeds bound")
	ErrFrameMalformed = errors.New("SecondBox Microsandbox helper frame is malformed")
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
