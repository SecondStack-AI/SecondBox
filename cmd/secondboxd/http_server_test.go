package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/config"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestPublicHTTPServerDeliversBufferedExecResultAfterConnectionTimeout(t *testing.T) {
	const connectionTimeout = 20 * time.Millisecond
	const execDuration = 100 * time.Millisecond
	stdout := []byte("completed stdout")
	stderr := []byte("completed stderr")
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/sandboxes/sbx_timeout/exec" {
			t.Errorf("buffered Exec request = %s %s", request.Method, request.URL.Path)
			http.Error(writer, "unexpected request", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read buffered Exec request: %v", err)
			return
		}
		if len(body) == 0 {
			t.Error("buffered Exec request body is empty")
			return
		}
		time.Sleep(execDuration)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(
			writer,
			`{"kind":"exited","exitCode":17,"elapsedMilliseconds":100,"output":{"stdoutBase64":%q,"stderrBase64":%q}}`,
			base64.StdEncoding.EncodeToString(stdout),
			base64.StdEncoding.EncodeToString(stderr),
		)
	})
	server := newPublicHTTPServer(config.Config{
		ListenAddress: "127.0.0.1:0",
		HTTPTimeout:   connectionTimeout,
	}, handler)
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			t.Errorf("shutdown buffered Exec test server: %v", err)
		}
		if err := <-serverErrors; err != nil && err != http.ErrServerClosed {
			t.Errorf("serve buffered Exec test server: %v", err)
		}
	})

	client, err := secondboxclient.NewSecondBoxSubjectClient(
		"http://"+listener.Addr().String(), "test-token", "test-tenant", "test-subject",
		&http.Client{Timeout: time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	handle := secondboxclient.NewSandboxHandle(client, contracts.Sandbox{
		ID: "sbx_timeout", Generation: 1,
	})
	outcome, err := handle.Execute(t.Context(), secondboxclient.BufferedExecRequest{
		Command: secondboxclient.Command{ShellCommand: &secondboxclient.ShellCommand{
			Mode: "shell", Command: "sleep 0.1; exit 17",
		}},
		Environment:          secondboxclient.StringMap{},
		DeadlineMilliseconds: 500,
		MaximumOutputBytes:   1024,
	}, "buffered-exec-timeout-regression", "")
	if err != nil {
		t.Fatal(err)
	}
	result, resultErr := secondboxclient.DecodeExecOutcome(outcome)
	if resultErr == nil {
		t.Fatal("non-zero buffered Exec result did not preserve its exit error")
	}
	if result.ExitCode != 17 || result.ElapsedMilliseconds != 100 ||
		string(result.Stdout) != string(stdout) || string(result.Stderr) != string(stderr) {
		t.Fatalf("buffered Exec result = %#v", result)
	}
}

func TestPublicHTTPServerBoundsSlowResponseWrite(t *testing.T) {
	const responseWriteTimeout = 50 * time.Millisecond
	writeResult := make(chan error, 1)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, err := writer.Write(make([]byte, 32<<20))
		writeResult <- err
	})
	server := newPublicHTTPServer(config.Config{
		ListenAddress: "127.0.0.1:0",
		HTTPTimeout:   responseWriteTimeout,
	}, handler)
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	listener = smallWriteBufferListener{Listener: listener}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			t.Errorf("shutdown slow response test server: %v", err)
		}
		if err := <-serverErrors; err != nil && err != http.ErrServerClosed {
			t.Errorf("serve slow response test server: %v", err)
		}
	})

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := fmt.Fprintf(client, "GET /slow-response HTTP/1.1\r\nHost: secondbox.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeResult:
		if err == nil {
			t.Fatal("slow response write completed without its deadline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow response write remained blocked after its deadline")
	}
}

type smallWriteBufferListener struct {
	net.Listener
}

func (listener smallWriteBufferListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tcpConnection, ok := connection.(*net.TCPConn)
	if !ok {
		_ = connection.Close()
		return nil, errors.New("SecondBox slow response test listener requires a TCP connection")
	}
	if err := tcpConnection.SetWriteBuffer(1024); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("SecondBox slow response test write buffer: %w", err)
	}
	return connection, nil
}
