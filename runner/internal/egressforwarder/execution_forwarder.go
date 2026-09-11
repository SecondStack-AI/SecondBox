// Package egressforwarder owns the network lifetime of one attributed generation.
package egressforwarder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
)

// ForwardAttributedExecution takes ownership of listener. The backend must restrict
// access to this generation before opening guest networking and wait for return
// before releasing its network resources. A gateway setup or listener failure
// terminates forwarding; a closed guest stream ends only that connection.
func ForwardAttributedExecution(parent context.Context, listener *net.TCPListener, gatewaySocket string, attribution egressattribution.ExecutionAttribution, maxConnections int) error {
	if listener == nil {
		return errors.New("SecondBox execution forwarder requires a listener")
	}
	defer listener.Close()
	if !filepath.IsAbs(gatewaySocket) || maxConnections < 1 {
		return errors.New("SecondBox execution forwarder configuration is invalid")
	}
	if err := egressattribution.WriteExecutionAttribution(io.Discard, attribution); err != nil {
		return err
	}
	ctx, cancel := context.WithDeadlineCause(parent, attribution.ExpiresAt, context.DeadlineExceeded)
	defer cancel()
	ctx, fail := context.WithCancelCause(ctx)
	defer fail(nil)
	stopListener := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopListener()
	var relays sync.WaitGroup
	defer relays.Wait()
	slots := make(chan struct{}, maxConnections)
	for {
		guest, err := listener.AcceptTCP()
		if err != nil {
			if ctx.Err() == nil {
				fail(fmt.Errorf("SecondBox execution forwarder accept failed: %w", err))
			}
			return context.Cause(ctx)
		}
		select {
		case slots <- struct{}{}:
			if ctx.Err() != nil {
				_ = guest.Close()
				return context.Cause(ctx)
			}
		default:
			// Capacity refusal closes the new connection without acquiring authority.
			_ = guest.Close()
			continue
		}
		relays.Add(1)
		go func() {
			defer relays.Done()
			defer func() { <-slots }()
			if err := relayAttributedExecution(ctx, guest, gatewaySocket, attribution); err != nil {
				fail(err)
			}
		}()
	}
}

func relayAttributedExecution(ctx context.Context, guest *net.TCPConn, gatewaySocket string, attribution egressattribution.ExecutionAttribution) error {
	defer guest.Close()
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", gatewaySocket)
	if err != nil {
		return fmt.Errorf("SecondBox execution forwarder gateway connection failed: %w", err)
	}
	gateway := connection.(*net.UnixConn)
	defer gateway.Close()
	stopRelay := context.AfterFunc(ctx, func() {
		_ = guest.Close()
		_ = gateway.Close()
	})
	defer stopRelay()
	for _, connection := range []net.Conn{guest, gateway} {
		if err := connection.SetDeadline(attribution.ExpiresAt); err != nil {
			return fmt.Errorf("SecondBox execution forwarder deadline failed: %w", err)
		}
	}
	if err := egressattribution.WriteExecutionAttribution(gateway, attribution); err != nil {
		return err
	}
	completed := make(chan error, 2)
	copyBytes := func(destination interface {
		net.Conn
		CloseWrite() error
	}, source net.Conn) {
		_, err := io.CopyBuffer(destination, source, make([]byte, 32*1024))
		if err == nil {
			err = destination.CloseWrite()
		}
		completed <- err
	}
	go copyBytes(gateway, guest)
	go copyBytes(guest, gateway)
	first := <-completed
	if first != nil {
		_ = guest.Close()
		_ = gateway.Close()
	}
	second := <-completed
	if first != nil && errors.Is(second, net.ErrClosed) {
		// Closing both sockets above interrupts the other copy.
		second = nil
	}
	for _, result := range []*error{&first, &second} {
		if errors.Is(*result, syscall.ECONNRESET) || errors.Is(*result, syscall.EPIPE) || errors.Is(*result, syscall.ENOTCONN) {
			// A peer can abandon this stream without revoking the generation.
			*result = nil
		}
	}
	if err := errors.Join(first, second); err != nil {
		return fmt.Errorf("SecondBox execution forwarder relay failed: %w", err)
	}
	return nil
}
