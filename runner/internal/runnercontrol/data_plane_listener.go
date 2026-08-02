package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// directPortHandshakeTimeout bounds how long an unauthenticated peer may
	// hold a Runner goroutine before presenting a credential.
	directPortHandshakeTimeout = 5 * time.Second
	// directPortAdmissionTimeout bounds the single control-plane consumption
	// round trip per caller connection.
	directPortAdmissionTimeout = 15 * time.Second
	// directPortAdmittingFrameWait bounds how long a caller may wait for its
	// admitting frame to reach the Runner. It stays inside the handshake
	// deadline so an unknown credential is still denied promptly.
	directPortAdmittingFrameWait = 3 * time.Second
	// directPortChunkBytes is the copy granularity on both legs of the bridge.
	directPortChunkBytes = 64 << 10
	// maximumLiveDirectPortConnections bounds concurrent caller-facing sockets.
	maximumLiveDirectPortConnections = 1024
)

// validateDataPlaneAddress rejects anything that is not a host:port pair.
//
// A listen address is a bind specification, so a wildcard host and an ephemeral
// port zero are both valid. An advertised address is handed to an ingress as a
// dialable address, so it must name a reachable host and a fixed port. Keeping
// the two settings independent is deliberate: the deployment decides how the
// bound socket is reachable, and the Runner never infers one from the other.
func validateDataPlaneAddress(name string, value string, listen bool) error {
	if value == "" {
		return fmt.Errorf("SecondBox runner protocol config requires %s", name)
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("%s must be a host:port address", name)
	}
	number, err := strconv.Atoi(port)
	minimumPort := 1
	if listen {
		minimumPort = 0
	}
	if err != nil || number < minimumPort || number > 65535 {
		return fmt.Errorf("%s must name a valid port", name)
	}
	if strings.TrimSpace(host) == "" && !listen {
		return fmt.Errorf("%s must name an explicit reachable host", name)
	}
	return nil
}

// dataPlaneListener owns the caller-facing Port socket and its readiness.
//
// Readiness is a Runner-local fact rather than backend evidence: the listener
// belongs to the protocol service, not to the compute backend, so the service
// projects it onto the registration and heartbeat capability report.
type dataPlaneListener struct {
	mu       sync.Mutex
	listener net.Listener
	failed   bool
	live     int
}

func newDataPlaneListener() *dataPlaneListener {
	return &dataPlaneListener{}
}

func (d *dataPlaneListener) bind(address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		d.mu.Lock()
		d.failed = true
		d.mu.Unlock()
		return fmt.Errorf("SecondBox runner data-plane listener bind %q: %w", address, err)
	}
	d.mu.Lock()
	d.listener, d.failed = listener, false
	d.mu.Unlock()
	return nil
}

func (d *dataPlaneListener) close() error {
	d.mu.Lock()
	listener := d.listener
	d.listener = nil
	d.mu.Unlock()
	if listener == nil {
		return nil
	}
	return listener.Close()
}

// ready reports whether the caller-facing transport can still admit work.
func (d *dataPlaneListener) ready() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.listener != nil && !d.failed
}

func (d *dataPlaneListener) markFailed() {
	d.mu.Lock()
	d.failed = true
	d.mu.Unlock()
}

func (d *dataPlaneListener) address() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listener == nil {
		return ""
	}
	return d.listener.Addr().String()
}

func (d *dataPlaneListener) admitConnection() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.live >= maximumLiveDirectPortConnections {
		return false
	}
	d.live++
	return true
}

func (d *dataPlaneListener) releaseConnection() {
	d.mu.Lock()
	if d.live > 0 {
		d.live--
	}
	d.mu.Unlock()
}

// startDataPlaneListener binds the caller-facing transport before the first
// registration so an unavailable listener never advertises capacity.
func (s *RunnerProtocolService) startDataPlaneListener(ctx context.Context) (func() error, error) {
	if err := s.dataPlane.bind(s.config.DataPlaneListenAddress); err != nil {
		return nil, err
	}
	slog.Info(
		"SecondBox runner data-plane listener bound",
		"listenAddress", s.dataPlane.address(),
		"advertisedAddress", s.config.DataPlaneAdvertisedAddress,
	)
	failures := make(chan error, 1)
	var accepting sync.WaitGroup
	accepting.Add(1)
	go func() {
		defer accepting.Done()
		s.acceptDataPlaneConnections(ctx, failures)
	}()
	s.setDataPlaneFailures(failures)
	return func() error {
		err := s.dataPlane.close()
		accepting.Wait()
		return err
	}, nil
}

func (s *RunnerProtocolService) acceptDataPlaneConnections(
	ctx context.Context,
	failures chan<- error,
) {
	var serving sync.WaitGroup
	defer serving.Wait()
	for {
		s.dataPlane.mu.Lock()
		listener := s.dataPlane.listener
		s.dataPlane.mu.Unlock()
		if listener == nil {
			return
		}
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// A listener that cannot accept is an unready Runner. Reporting the
			// failure drops the control connection, which fences every active
			// instance exactly as an unavailable network-policy listener does.
			s.dataPlane.markFailed()
			select {
			case failures <- fmt.Errorf("SecondBox runner data-plane accept: %w", err):
			default:
			}
			return
		}
		if !s.dataPlane.admitConnection() {
			_ = connection.Close()
			continue
		}
		serving.Add(1)
		go func() {
			defer serving.Done()
			defer s.dataPlane.releaseConnection()
			s.serveDirectPortConnection(ctx, connection)
		}()
	}
}

func (s *RunnerProtocolService) setDataPlaneFailures(failures chan error) {
	s.stateMu.Lock()
	s.dataPlaneFailures = failures
	s.stateMu.Unlock()
}

func (s *RunnerProtocolService) dataPlaneFailureSource() <-chan error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.dataPlaneFailures
}
