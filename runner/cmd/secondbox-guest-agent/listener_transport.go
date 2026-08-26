package main

import (
	"errors"
	"fmt"
	"net"
	"os"
)

// listenerTransport is the single selected listener family for both guest
// services. vsock is the microVM transport; Unix sockets are the gVisor
// transport, where the runner passes a socket directory through the gofer.
type listenerTransport struct {
	controlVsockPort   int
	protocolVsockPort  int
	controlUnixSocket  string
	protocolUnixSocket string
}

// unixSocketPathBound keeps paths inside sockaddr_un's sun_path limit.
const unixSocketPathBound = 107

// selectListenerTransport requires exactly one complete transport family:
// both vsock ports, or both Unix socket paths. Mixed or absent selection is
// a startup failure, never a fallback.
func selectListenerTransport(
	controlVsockPort, protocolVsockPort int,
	controlUnixSocket, protocolUnixSocket string,
) (listenerTransport, error) {
	vsockSelected := controlVsockPort != 0 || protocolVsockPort != 0
	unixSelected := controlUnixSocket != "" || protocolUnixSocket != ""
	switch {
	case vsockSelected && unixSelected:
		return listenerTransport{}, errors.New("vsock ports and Unix socket paths are mutually exclusive")
	case vsockSelected:
		if controlVsockPort < 1 || controlVsockPort > 65535 ||
			protocolVsockPort < 1 || protocolVsockPort > 65535 ||
			controlVsockPort == protocolVsockPort {
			return listenerTransport{}, errors.New(
				"guest control and protocol vsock ports must be explicit, distinct values from 1 through 65535")
		}
		return listenerTransport{controlVsockPort: controlVsockPort, protocolVsockPort: protocolVsockPort}, nil
	case unixSelected:
		if controlUnixSocket == "" || protocolUnixSocket == "" || controlUnixSocket == protocolUnixSocket {
			return listenerTransport{}, errors.New(
				"guest control and protocol Unix sockets must be explicit, distinct paths")
		}
		for _, path := range []string{controlUnixSocket, protocolUnixSocket} {
			if len(path) > unixSocketPathBound {
				return listenerTransport{}, fmt.Errorf("guest Unix socket path exceeds %d bytes", unixSocketPathBound)
			}
		}
		return listenerTransport{controlUnixSocket: controlUnixSocket, protocolUnixSocket: protocolUnixSocket}, nil
	default:
		return listenerTransport{}, errors.New("guest listeners require vsock ports or Unix socket paths")
	}
}

func (transport listenerTransport) listenControl() (net.Listener, error) {
	if transport.controlUnixSocket != "" {
		return listenUnixSocket(transport.controlUnixSocket)
	}
	return listenSocket(transport.controlVsockPort)
}

func (transport listenerTransport) listenProtocol() (net.Listener, error) {
	if transport.protocolUnixSocket != "" {
		return listenUnixSocket(transport.protocolUnixSocket)
	}
	return listenSocket(transport.protocolVsockPort)
}

// listenUnixSocket removes a stale socket, listens, and restricts the socket
// mode; the runner controls the surrounding directory's ownership.
func listenUnixSocket(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale guest socket %s: %w", path, err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on guest socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("restrict guest socket %s: %w", path, err), listener.Close())
	}
	return listener, nil
}
