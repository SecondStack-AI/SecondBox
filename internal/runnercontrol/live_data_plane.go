package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"sync"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

var ErrLiveDataPlaneUnavailable = errors.New("SecondBox live data-plane transport is unavailable")

type liveDataPlaneDelivery struct {
	message *runnerv1.RunnerToControlPlane
	err     error
}

type liveRunnerConnection struct {
	connectionID string
	sender       controlPlaneFrameSender
	session      *Session
	mu           sync.Mutex
}

type liveDataPlaneRoute struct {
	runnerID  string
	key       string
	delivery  chan liveDataPlaneDelivery
	closed    chan struct{}
	closeErr  error
	closeOnce sync.Once
}

// LiveDataPlaneBroker routes Exec, PTY, and File frames through the process that owns
// the authenticated Runner connection. It retains only bounded channel entries
// and never writes a payload to PostgreSQL.
type LiveDataPlaneBroker struct {
	mu          sync.Mutex
	connections map[string]*liveRunnerConnection
	routes      map[string]*liveDataPlaneRoute
}

func NewLiveDataPlaneBroker() *LiveDataPlaneBroker {
	return &LiveDataPlaneBroker{
		connections: make(map[string]*liveRunnerConnection),
		routes:      make(map[string]*liveDataPlaneRoute),
	}
}

// AttachConnection binds the authenticated process-local Runner stream used by
// proxied Exec and File sessions.
func (broker *LiveDataPlaneBroker) AttachConnection(
	runnerID string,
	connectionID string,
	sender LiveDataPlaneSender,
	session *Session,
) (func(), error) {
	if runnerID == "" || connectionID == "" || sender == nil || session == nil {
		return nil, errors.New("SecondBox live data-plane connection is incomplete")
	}
	broker.bind(runnerID, connectionID, sender, session)
	return func() { broker.unbind(runnerID, connectionID) }, nil
}

func (broker *LiveDataPlaneBroker) bind(
	runnerID string,
	connectionID string,
	sender controlPlaneFrameSender,
	session *Session,
) {
	broker.mu.Lock()
	selected := broker.removeRunnerRoutesLocked(runnerID)
	broker.connections[runnerID] = &liveRunnerConnection{
		connectionID: connectionID, sender: sender, session: session,
	}
	broker.mu.Unlock()
	closeLiveDataPlaneRoutes(selected, ErrLiveDataPlaneUnavailable)
}

func (broker *LiveDataPlaneBroker) unbind(runnerID string, connectionID string) {
	broker.mu.Lock()
	connection := broker.connections[runnerID]
	if connection == nil || connection.connectionID != connectionID {
		broker.mu.Unlock()
		return
	}
	delete(broker.connections, runnerID)
	selected := broker.removeRunnerRoutesLocked(runnerID)
	broker.mu.Unlock()
	closeLiveDataPlaneRoutes(selected, ErrLiveDataPlaneUnavailable)
}

func (broker *LiveDataPlaneBroker) removeRunnerRoutesLocked(runnerID string) []*liveDataPlaneRoute {
	selected := make([]*liveDataPlaneRoute, 0)
	for key, route := range broker.routes {
		if route.runnerID == runnerID {
			selected = append(selected, route)
			delete(broker.routes, key)
		}
	}
	return selected
}

func closeLiveDataPlaneRoutes(routes []*liveDataPlaneRoute, err error) {
	for _, route := range routes {
		route.close(err)
	}
}

func (route *liveDataPlaneRoute) close(err error) {
	route.closeOnce.Do(func() {
		route.closeErr = err
		close(route.closed)
	})
}

// LiveDataPlaneStream owns one in-memory proxied Exec, PTY, or File route.
type LiveDataPlaneStream struct {
	broker *LiveDataPlaneBroker
	route  *liveDataPlaneRoute
	once   sync.Once
}

func (broker *LiveDataPlaneBroker) Open(
	runnerID string,
	kind string,
	operationID string,
	streamID string,
) (*LiveDataPlaneStream, error) {
	key, err := liveDataPlaneKey(kind, operationID, streamID)
	if err != nil {
		return nil, err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.connections[runnerID] == nil {
		return nil, ErrLiveDataPlaneUnavailable
	}
	if broker.routes[key] != nil {
		return nil, errors.New("SecondBox live data-plane session is already attached")
	}
	route := &liveDataPlaneRoute{
		runnerID: runnerID, key: key, delivery: make(chan liveDataPlaneDelivery, 16),
		closed: make(chan struct{}),
	}
	broker.routes[key] = route
	return &LiveDataPlaneStream{broker: broker, route: route}, nil
}

func (stream *LiveDataPlaneStream) Send(message *runnerv1.ControlPlaneToRunner) error {
	if stream == nil || stream.broker == nil || stream.route == nil || message == nil {
		return ErrLiveDataPlaneUnavailable
	}
	stream.broker.mu.Lock()
	if stream.broker.routes[stream.route.key] != stream.route {
		stream.broker.mu.Unlock()
		return ErrLiveDataPlaneUnavailable
	}
	connection := stream.broker.connections[stream.route.runnerID]
	stream.broker.mu.Unlock()
	if connection == nil {
		return ErrLiveDataPlaneUnavailable
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if err := connection.session.ValidateOutboundRelayFrame(message); err != nil {
		return err
	}
	if err := connection.sender.Send(message); err != nil {
		return fmt.Errorf("SecondBox live data-plane send: %w", err)
	}
	return nil
}

func (stream *LiveDataPlaneStream) Receive(ctx context.Context) (*runnerv1.RunnerToControlPlane, error) {
	if stream == nil || stream.route == nil {
		return nil, ErrLiveDataPlaneUnavailable
	}
	select {
	case delivery := <-stream.route.delivery:
		return delivery.message, delivery.err
	case <-stream.route.closed:
		return nil, stream.route.closeErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (stream *LiveDataPlaneStream) Close() {
	if stream == nil {
		return
	}
	stream.once.Do(func() {
		stream.broker.mu.Lock()
		if stream.broker.routes[stream.route.key] == stream.route {
			delete(stream.broker.routes, stream.route.key)
		}
		stream.broker.mu.Unlock()
		stream.route.close(ErrLiveDataPlaneUnavailable)
	})
}

// Deliver routes one already validated Runner Exec or File event in memory.
func (broker *LiveDataPlaneBroker) Deliver(
	ctx context.Context,
	event Event,
) error {
	kind, operationID, streamID, err := liveDataPlaneMessageIdentity(event.Message)
	if err != nil {
		return err
	}
	key, err := liveDataPlaneKey(kind, operationID, streamID)
	if err != nil {
		return err
	}
	broker.mu.Lock()
	route := broker.routes[key]
	broker.mu.Unlock()
	if route == nil || route.runnerID != event.RunnerID {
		return ErrLiveDataPlaneUnavailable
	}
	select {
	case route.delivery <- liveDataPlaneDelivery{message: event.Message}:
		return nil
	case <-route.closed:
		return ErrLiveDataPlaneUnavailable
	case <-ctx.Done():
		return ctx.Err()
	}
}

func liveDataPlaneKey(kind string, operationID string, streamID string) (string, error) {
	if (kind != "exec" && kind != "terminal" && kind != "file") || operationID == "" || streamID == "" {
		return "", errors.New("SecondBox live data-plane identity is incomplete")
	}
	return kind + "\x00" + operationID + "\x00" + streamID, nil
}

func liveDataPlaneMessageIdentity(
	message *runnerv1.RunnerToControlPlane,
) (string, string, string, error) {
	if frame := message.GetExec(); frame != nil {
		return "exec", frame.OperationId, frame.StreamId, nil
	}
	if frame := message.GetFile(); frame != nil {
		return "file", frame.OperationId, frame.StreamId, nil
	}
	if frame := message.GetPty(); frame != nil {
		return "terminal", frame.OperationId, frame.StreamId, nil
	}
	return "", "", "", errors.New("SecondBox live data-plane message is not Exec, PTY, or File")
}
