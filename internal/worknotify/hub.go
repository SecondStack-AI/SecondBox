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
	nextID        uint64
	subscriptions map[subscriptionKey]map[uint64]chan struct{}
}

// NewHub constructs an empty wakeup hub.
func NewHub() *Hub {
	return &Hub{
		subscriptions: make(map[subscriptionKey]map[uint64]chan struct{}),
	}
}

// Subscribe returns a capacity-one channel and an idempotent cancellation function.
func (hub *Hub) Subscribe(kind Kind, key string) (<-chan struct{}, func()) {
	hub.mu.Lock()
	hub.nextID++
	id := hub.nextID
	subscription := subscriptionKey{kind: kind, key: key}
	wakeups := make(chan struct{}, 1)
	if hub.subscriptions[subscription] == nil {
		hub.subscriptions[subscription] = make(map[uint64]chan struct{})
	}
	hub.subscriptions[subscription][id] = wakeups
	hub.mu.Unlock()

	var cancelOnce sync.Once
	return wakeups, func() {
		cancelOnce.Do(func() {
			hub.mu.Lock()
			delete(hub.subscriptions[subscription], id)
			if len(hub.subscriptions[subscription]) == 0 {
				delete(hub.subscriptions, subscription)
			}
			hub.mu.Unlock()
		})
	}
}

// Publish offers one non-blocking hint to every matching subscriber.
func (hub *Hub) Publish(kind Kind, key string) {
	hub.mu.Lock()
	for _, wakeups := range hub.subscriptions[subscriptionKey{kind: kind, key: key}] {
		select {
		case wakeups <- struct{}{}:
		default:
		}
	}
	hub.mu.Unlock()
}
