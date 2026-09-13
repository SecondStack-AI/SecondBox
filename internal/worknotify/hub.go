// Package worknotify turns PostgreSQL commit notifications into process-local,
// coalesced wakeup hints. Durable tables remain the only work authority.
package worknotify

import (
	"sync"
)

// Kind identifies one durable worker queue.
type Kind string

const (
	KindLifecycle     Kind = "lifecycle"
	KindAssignment    Kind = "assignment"
	KindRunnerCommand Kind = "runner_command"
)

type subscriptionKey struct {
	kind Kind
	key  string
}

// Source provides bounded, coalesced wakeup subscriptions.
type Source interface {
	Subscribe(Kind, string) (<-chan struct{}, func())
}

// Hub fans commit hints out to interested workers without carrying authority.
type Hub struct {
	mu            sync.Mutex
	subscriptions map[subscriptionKey]map[chan struct{}]struct{}
}

// NewHub constructs an empty wakeup hub.
func NewHub() *Hub {
	return &Hub{
		subscriptions: make(map[subscriptionKey]map[chan struct{}]struct{}),
	}
}

// Subscribe returns a capacity-one channel and an idempotent cancellation function.
func (hub *Hub) Subscribe(kind Kind, key string) (<-chan struct{}, func()) {
	hub.mu.Lock()
	subscription := subscriptionKey{kind: kind, key: key}
	wakeups := make(chan struct{}, 1)
	if hub.subscriptions[subscription] == nil {
		hub.subscriptions[subscription] = make(map[chan struct{}]struct{})
	}
	hub.subscriptions[subscription][wakeups] = struct{}{}
	hub.mu.Unlock()

	return wakeups, func() {
		hub.mu.Lock()
		delete(hub.subscriptions[subscription], wakeups)
		if len(hub.subscriptions[subscription]) == 0 {
			delete(hub.subscriptions, subscription)
		}
		hub.mu.Unlock()
	}
}

// Publish offers one non-blocking hint to every matching subscriber.
func (hub *Hub) Publish(kind Kind, key string) {
	hub.mu.Lock()
	for wakeups := range hub.subscriptions[subscriptionKey{kind: kind, key: key}] {
		select {
		case wakeups <- struct{}{}:
		default:
		}
	}
	hub.mu.Unlock()
}
