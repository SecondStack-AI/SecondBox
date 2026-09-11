package egressattribution

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func testExecutionAttribution() ExecutionAttribution {
	return ExecutionAttribution{
		TenantRef: "tenant", SubjectRef: "subject", SandboxID: "sandbox", InstanceID: "instance",
		AssignmentID: "assignment", Generation: 7, AuthorizationRef: "application-command",
		ExpiresAt: time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond),
	}
}

func TestExecutionAttributionPreservesGuestBytesOnUnixConnection(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "gateway.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	expected := testExecutionAttribution()
	guest := []byte("CONNECT api.example.com:443 HTTP/1.1\r\nX-Execution-Identity: forged\r\n\r\n")
	sent := make(chan error, 1)
	go func() {
		connection, err := net.Dial("unix", listener.Addr().String())
		if err != nil {
			sent <- err
			return
		}
		defer connection.Close()
		if err = connection.SetDeadline(time.Now().Add(5 * time.Second)); err == nil {
			err = WriteExecutionAttribution(connection, expected)
		}
		if err == nil {
			_, err = connection.Write(guest)
		}
		sent <- err
	}()
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	actual, err := ReadExecutionAttribution(connection, time.Now())
	if err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("attribution = %#v, error = %v", actual, err)
	}
	remaining, err := io.ReadAll(connection)
	if err != nil || !bytes.Equal(remaining, guest) {
		t.Fatalf("guest bytes = %q, error = %v", remaining, err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}

func TestExecutionAttributionRejectsMalformedOrExpiredFrames(t *testing.T) {
	valid := testExecutionAttribution()
	payload, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	frame := func(payload []byte) []byte {
		header := make([]byte, len(executionAttributionMagic)+4)
		copy(header, executionAttributionMagic)
		binary.BigEndian.PutUint32(header[len(executionAttributionMagic):], uint32(len(payload)))
		return append(header, payload...)
	}
	oversized := frame(nil)
	binary.BigEndian.PutUint32(oversized[len(executionAttributionMagic):], maximumExecutionAttributionBytes+1)
	unknown := append(append([]byte{}, payload[:len(payload)-1]...), []byte(`,"credentialRef":"forged"}`)...)
	cases := map[string][]byte{
		"ordinary HTTP":     []byte("CONNECT api.example.com:443 HTTP/1.1\r\n\r\n"),
		"short header":      []byte(executionAttributionMagic),
		"oversized":         oversized,
		"empty":             frame(nil),
		"truncated payload": frame(payload)[:len(frame(payload))-1],
		"unknown authority": frame(unknown),
		"trailing JSON":     frame(append(append([]byte{}, payload...), []byte("{}")...)),
		"missing identity":  frame([]byte("{}")),
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadExecutionAttribution(bytes.NewReader(wire), time.Now()); err == nil {
				t.Fatal("malformed attribution accepted")
			}
		})
	}
	if _, err := ReadExecutionAttribution(bytes.NewReader(frame(payload)), valid.ExpiresAt); err == nil {
		t.Fatal("expired attribution accepted")
	}
	for _, invalid := range []ExecutionAttribution{
		{},
		{TenantRef: "tenant", SubjectRef: "subject", SandboxID: "sandbox", InstanceID: "instance", AssignmentID: "assignment", Generation: 0, AuthorizationRef: "ref", ExpiresAt: valid.ExpiresAt},
		{TenantRef: "tenant", SubjectRef: "subject", SandboxID: "sandbox", InstanceID: "instance", AssignmentID: "assignment", Generation: 1, AuthorizationRef: "ref\ninjected", ExpiresAt: valid.ExpiresAt},
	} {
		var wire bytes.Buffer
		if err := WriteExecutionAttribution(&wire, invalid); err == nil || wire.Len() != 0 {
			t.Fatal("invalid attribution emitted bytes")
		}
	}
}
