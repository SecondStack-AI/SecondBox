// Package imagecredentials keeps registry pull credentials out of durable commands.
package imagecredentials

import (
	"errors"
	"sync"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

const maximumCredentialBindings = 1024

type PullCredentials struct {
	Username string
	Token    string
}

// EnrichRunnerCommand adds an operation-scoped secret only to the live stream message.
func (broker *Broker) EnrichRunnerCommand(message *runnerv1.ControlPlaneToRunner) error {
	assignment := message.GetAssignment()
	if assignment == nil || assignment.ExecutionImage == nil {
		return nil
	}
	operationID := assignment.GetCorrelation().GetOperationId()
	credentials, found := broker.Load(operationID)
	if !found {
		return nil
	}
	assignment.ExecutionImage.PullCredentials = &runnerv1.RegistryPullCredentials{
		Username: credentials.Username,
		Token:    credentials.Token,
	}
	return nil
}

type binding struct {
	credentials PullCredentials
	expiresAt   time.Time
}

// Broker stores bounded operation-scoped credentials for runner command delivery.
type Broker struct {
	mu       sync.Mutex
	now      func() time.Time
	lifetime time.Duration
	bindings map[string]binding
}

func NewBroker(now func() time.Time, lifetime time.Duration) (*Broker, error) {
	if now == nil || lifetime <= 0 {
		return nil, errors.New("SecondBox image credential broker requires a clock and positive lifetime")
	}
	return &Broker{now: now, lifetime: lifetime, bindings: make(map[string]binding)}, nil
}

func (broker *Broker) Store(operationID string, credentials PullCredentials) error {
	if operationID == "" || credentials.Token == "" {
		return errors.New("SecondBox image credential binding requires an Operation ID and pull token")
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.removeExpiredLocked()
	if _, exists := broker.bindings[operationID]; !exists && len(broker.bindings) >= maximumCredentialBindings {
		return errors.New("SecondBox image credential broker capacity is exhausted")
	}
	broker.bindings[operationID] = binding{
		credentials: credentials,
		expiresAt:   broker.now().UTC().Add(broker.lifetime),
	}
	return nil
}

func (broker *Broker) Load(operationID string) (PullCredentials, bool) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.removeExpiredLocked()
	value, found := broker.bindings[operationID]
	return value.credentials, found
}

func (broker *Broker) Delete(operationID string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	delete(broker.bindings, operationID)
}

func (broker *Broker) removeExpiredLocked() {
	now := broker.now().UTC()
	for operationID, value := range broker.bindings {
		if !value.expiresAt.After(now) {
			delete(broker.bindings, operationID)
		}
	}
}
