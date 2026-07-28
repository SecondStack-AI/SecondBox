package microvm

import (
	"context"
	"fmt"
	"testing"

	"agent-manager/internal/config"
	"agent-manager/internal/egressproxy"
	"agent-manager/internal/registry"
	"agent-manager/internal/runtimemanager"
)

func TestCompartmentIsolationGateDerivesDistinctCompartments(t *testing.T) {
	agentID := "iso-agent-1"
	dm, err := registry.DeriveCompartmentID(agentID, "slack", "team:T1", "slack:user:U1", "111.000", registry.CompartmentKindDirect)
	if err != nil {
		t.Fatalf("derive dm compartment: %v", err)
	}
	channel, err := registry.DeriveCompartmentID(agentID, "slack", "team:T1", "slack:channel:C1", "", registry.CompartmentKindShared)
	if err != nil {
		t.Fatalf("derive channel compartment: %v", err)
	}
	threadA, err := registry.DeriveCompartmentID(agentID, "slack", "team:T1", "slack:channel:C1", "111.000", registry.CompartmentKindShared)
	if err != nil {
		t.Fatalf("derive thread A compartment: %v", err)
	}
	threadB, err := registry.DeriveCompartmentID(agentID, "slack", "team:T1", "slack:channel:C1", "222.000", registry.CompartmentKindShared)
	if err != nil {
		t.Fatalf("derive thread B compartment: %v", err)
	}

	seen := map[string]string{}
	for label, id := range map[string]string{
		"dm":      dm,
		"channel": channel,
		"threadA": threadA,
		"threadB": threadB,
	} {
		if prior, ok := seen[id]; ok {
			t.Fatalf("%s and %s derived the same compartment %q", prior, label, id)
		}
		seen[id] = label
	}

	dmWithThread, err := registry.DeriveCompartmentID(agentID, "slack", "team:T1", "slack:user:U1", "222.000", registry.CompartmentKindDirect)
	if err != nil {
		t.Fatalf("derive dm compartment with thread: %v", err)
	}
	if dmWithThread == dm {
		t.Fatalf("direct compartments should keep thread keys distinct: got same compartment %q", dm)
	}
}

func TestCompartmentIsolationGateDistinctInstancesAndManualStart(t *testing.T) {
	m := &Manager{
		cfg:       &config.Config{MicroVMBridgeCIDR: "10.0.0.1/24"},
		instances: map[string]*instance{},
	}
	m.startCompartment = func(_ context.Context, agentID, compartmentID string, _ runtimemanager.StartOpts) (string, error) {
		inst := &instance{
			id:            fmt.Sprintf("fc-%s-%s", agentID, compartmentID),
			agentID:       agentID,
			compartmentID: compartmentID,
			socket:        fmt.Sprintf("/run/%s/%s/firecracker.sock", agentID, compartmentID),
			done:          make(chan struct{}),
		}
		m.mu.Lock()
		m.addInstanceLocked(inst)
		m.mu.Unlock()
		return inst.id, nil
	}

	aID, err := m.createAndStart(context.Background(), "agent-gate", runtimemanager.StartOpts{CompartmentID: "cmp_a"})
	if err != nil {
		t.Fatalf("create cmp_a: %v", err)
	}
	bID, err := m.createAndStart(context.Background(), "agent-gate", runtimemanager.StartOpts{CompartmentID: "cmp_b"})
	if err != nil {
		t.Fatalf("manual start cmp_b: %v", err)
	}
	if aID == bID {
		t.Fatalf("compartment A and B returned the same instance %q", aID)
	}
	a := m.lookup(aID)
	b := m.lookup(bID)
	if a == nil || b == nil {
		t.Fatalf("expected both compartments live, got a=%#v b=%#v", a, b)
	}
	if a.socket == "" || a.socket == b.socket {
		t.Fatalf("expected distinct compartment sockets, got a=%q b=%q", a.socket, b.socket)
	}
}

func TestCompartmentIsolationGateSourceIPBinding(t *testing.T) {
	sources := egressproxy.NewSourceRegistry()
	if err := sources.Register(egressproxy.SourceBinding{
		AgentID:       "agent-gate",
		CompartmentID: "cmp_a",
		ContainerID:   "fc-agent-gate-cmp-a",
		Generation:    "fc-agent-gate-cmp-a",
		SourceIP:      "172.30.0.2",
	}); err != nil {
		t.Fatalf("register cmp_a binding: %v", err)
	}
	if err := sources.Register(egressproxy.SourceBinding{
		AgentID:       "agent-gate",
		CompartmentID: "cmp_b",
		ContainerID:   "fc-agent-gate-cmp-b",
		Generation:    "fc-agent-gate-cmp-b",
		SourceIP:      "172.30.0.2",
	}); err == nil {
		t.Fatal("expected duplicate source IP to be rejected while cmp_a binding is live")
	}
	got, err := sources.ResolveSource("172.30.0.2:50000")
	if err != nil {
		t.Fatalf("resolve cmp_a source: %v", err)
	}
	if got.CompartmentID != "cmp_a" {
		t.Fatalf("resolved compartment = %q, want cmp_a", got.CompartmentID)
	}

	sources.UnregisterContainer("fc-agent-gate-cmp-a")
	if err := sources.Register(egressproxy.SourceBinding{
		AgentID:       "agent-gate",
		CompartmentID: "cmp_b",
		ContainerID:   "fc-agent-gate-cmp-b",
		Generation:    "fc-agent-gate-cmp-b",
		SourceIP:      "172.30.0.2",
	}); err != nil {
		t.Fatalf("register cmp_b after unregister cmp_a: %v", err)
	}
	got, err = sources.ResolveSource("172.30.0.2:50000")
	if err != nil {
		t.Fatalf("resolve cmp_b source: %v", err)
	}
	if got.CompartmentID != "cmp_b" {
		t.Fatalf("resolved compartment after reuse = %q, want cmp_b", got.CompartmentID)
	}
}
