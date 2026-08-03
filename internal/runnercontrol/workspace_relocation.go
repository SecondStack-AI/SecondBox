package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

type workspaceTransferAuthority interface {
	RouteWorkspaceTransfer(context.Context, string, *runnerv1.WorkspaceTransferFrame, time.Time) (string, error)
}

type workspaceTransferConnection struct {
	connectionID string
	sender       controlPlaneFrameSender
}

// WorkspaceTransferHub holds only live stream references. PostgreSQL validates
// authority and owns every durable relocation state transition.
type WorkspaceTransferHub struct {
	store       workspaceTransferAuthority
	mu          sync.RWMutex
	connections map[string]workspaceTransferConnection
}

// NewWorkspaceTransferHub constructs the process-local relay registry.
func NewWorkspaceTransferHub(store workspaceTransferAuthority) (*WorkspaceTransferHub, error) {
	if store == nil {
		return nil, errors.New("SecondBox Workspace transfer hub requires durable authority")
	}
	return &WorkspaceTransferHub{
		store: store, connections: make(map[string]workspaceTransferConnection),
	}, nil
}

func (hub *WorkspaceTransferHub) Register(
	runnerID string,
	connectionID string,
	sender controlPlaneFrameSender,
) {
	hub.mu.Lock()
	hub.connections[runnerID] = workspaceTransferConnection{
		connectionID: connectionID, sender: sender,
	}
	hub.mu.Unlock()
}

func (hub *WorkspaceTransferHub) Unregister(runnerID string, connectionID string) {
	hub.mu.Lock()
	if connection := hub.connections[runnerID]; connection.connectionID == connectionID {
		delete(hub.connections, runnerID)
	}
	hub.mu.Unlock()
}

func (hub *WorkspaceTransferHub) Handle(
	ctx context.Context,
	runnerID string,
	frame *runnerv1.WorkspaceTransferFrame,
	now time.Time,
) error {
	peerRunnerID, err := hub.store.RouteWorkspaceTransfer(ctx, runnerID, frame, now)
	if err != nil {
		return err
	}
	hub.mu.RLock()
	peer, connected := hub.connections[peerRunnerID]
	hub.mu.RUnlock()
	if !connected {
		return fmt.Errorf("SecondBox Workspace transfer peer Runner %q is disconnected", peerRunnerID)
	}
	if err := peer.sender.Send(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_WorkspaceTransfer{
			WorkspaceTransfer: frame,
		},
	}); err != nil {
		return fmt.Errorf("SecondBox Workspace transfer forward: %w", err)
	}
	return nil
}
