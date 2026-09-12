package secondboxclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ForwardPort owns listener until cancellation or failure. Every accepted TCP
// connection consumes a fresh PortSession credential; one renewed Lease fences
// the whole forwarding lifetime. All sessions are closed before Lease release.
func (handle *SandboxHandle) ForwardPort(ctx context.Context, listener net.Listener, policy PortPolicy, ready func(PortSession) error) (resultErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer listener.Close()
	keeper, err := handle.KeepLease(ctx, time.Minute)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, keeper.Close()) }()
	request := CreatePortSessionRequest{Name: policy.Name, DurationSeconds: min(policy.MaximumSessionSeconds, int64(86400))}
	initial, err := handle.CreatePortSession(ctx, request, "", keeper.ID())
	if err != nil {
		return err
	}
	initialOwned := true
	defer func() {
		if initialOwned {
			resultErr = errors.Join(resultErr, handle.closeForwardSession(initial))
		}
	}()
	if ready != nil {
		if err := ready(initial); err != nil {
			return err
		}
	}
	var workers sync.WaitGroup
	var failures []error
	var failureMu sync.Mutex
	fail := func(err error) {
		failureMu.Lock()
		failures = append(failures, err)
		failureMu.Unlock()
		cancel()
	}
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					fail(err)
				}
				return
			case <-ticker.C:
				if err := keeper.Err(); err != nil {
					fail(err)
				}
			}
		}
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				fail(fmt.Errorf("SecondBox Port listener accept: %w", err))
			}
			break
		}
		var initialSession *PortSession
		if initialOwned {
			copy := initial
			initialSession = &copy
			initialOwned = false
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := handle.forwardConnection(ctx, connection, request, keeper.ID(), initialSession); err != nil {
				fail(err)
			}
		}()
	}
	cancel()
	workers.Wait()
	<-monitorDone
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	return ctx.Err()
}

func (handle *SandboxHandle) closeForwardSession(session PortSession) error {
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return handle.ClosePortSession(cleanup, session.ID, "")
}

func (handle *SandboxHandle) forwardConnection(ctx context.Context, connection net.Conn, request CreatePortSessionRequest, leaseID string, initial *PortSession) (resultErr error) {
	defer connection.Close()
	// The listener may have been idle longer than the initial session's bound.
	if initial != nil && !time.Now().Before(initial.ExpiresAt) {
		if err := handle.closeForwardSession(*initial); err != nil {
			return err
		}
		initial = nil
	}
	var session PortSession
	if initial != nil {
		session = *initial
	} else {
		var err error
		session, err = handle.CreatePortSession(ctx, request, "", leaseID)
		if err != nil {
			return err
		}
	}
	defer func() { resultErr = errors.Join(resultErr, handle.closeForwardSession(session)) }()
	tunnel, err := handle.ConnectPortTunnel(ctx, session, nil, nil)
	if err != nil {
		return err
	}
	// A tunnel has no half-close protocol. Either EOF closes both directions;
	// wait for both pumps so no goroutine outlives its PortSession or Lease.
	done := make(chan error, 2)
	go func() { _, err := io.Copy(tunnel, connection); done <- err }()
	go func() { _, err := io.Copy(connection, tunnel); done <- err }()
	remaining := 2
	select {
	case err := <-done:
		resultErr = forwardStreamError(err)
		remaining--
	case <-ctx.Done():
	}
	resultErr = errors.Join(resultErr, forwardStreamError(connection.Close()), forwardStreamError(tunnel.Close()))
	for range remaining {
		resultErr = errors.Join(resultErr, forwardStreamError(<-done))
	}
	return resultErr
}

func forwardStreamError(err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return nil
	}
	return fmt.Errorf("SecondBox Port forwarding stream: %w", err)
}
