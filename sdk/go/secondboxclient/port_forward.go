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

// ForwardPort owns listener until cancellation or a listener or Lease failure.
// Connection failures are returned when forwarding ends and do not stop peers.
// Every accepted TCP connection consumes a fresh PortSession credential; one
// renewed Lease fences the whole lifetime. Sessions close before Lease release.
func (handle *SandboxHandle) ForwardPort(ctx context.Context, listener net.Listener, policy PortPolicy, ready func(PortSession) error) (resultErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer listener.Close()
	keeper, err := handle.KeepLease(ctx, time.Minute)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, keeper.Close()) }()
	initial, err := handle.createForwardSession(ctx, policy, keeper)
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
	recordFailure := func(err error) {
		failureMu.Lock()
		failures = append(failures, err)
		failureMu.Unlock()
	}
	fail := func(err error) {
		recordFailure(err)
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
			if err := handle.forwardConnection(ctx, connection, policy, keeper, initialSession); err != nil {
				recordFailure(err)
			}
		}()
	}
	cancel()
	workers.Wait()
	<-monitorDone
	if len(failures) != 0 {
		return errors.Join(append(failures, ctx.Err())...)
	}
	return ctx.Err()
}

// Leave time for admission after the request crosses the network. Each request
// reads the latest service-granted expiry, including successful Lease renewals.
func (handle *SandboxHandle) createForwardSession(ctx context.Context, policy PortPolicy, keeper *LeaseKeeper) (PortSession, error) {
	keeper.mu.Lock()
	expiresAt, leaseID, failure := keeper.lease.ExpiresAt, keeper.lease.ID, keeper.failure
	keeper.mu.Unlock()
	if failure != nil {
		return PortSession{}, fmt.Errorf("SecondBox Port forwarding Lease: %w", failure)
	}
	seconds := min(int64((time.Until(expiresAt)-time.Second)/time.Second), policy.MaximumSessionSeconds, int64(86400))
	if seconds < 1 {
		return PortSession{}, errors.New("SecondBox Port forwarding Lease has insufficient remaining lifetime")
	}
	return handle.CreatePortSession(ctx, CreatePortSessionRequest{Name: policy.Name, DurationSeconds: seconds}, "", leaseID)
}

func (handle *SandboxHandle) closeForwardSession(session PortSession) error {
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return handle.ClosePortSession(cleanup, session.ID, "")
}

func (handle *SandboxHandle) forwardConnection(ctx context.Context, connection net.Conn, policy PortPolicy, keeper *LeaseKeeper, initial *PortSession) (resultErr error) {
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
		session, err = handle.createForwardSession(ctx, policy, keeper)
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
