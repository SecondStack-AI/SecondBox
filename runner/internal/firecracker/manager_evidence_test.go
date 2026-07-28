package firecracker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
)

type recordingManagerEvidenceSink struct {
	mu      sync.Mutex
	records []runnerevidence.Record
}

func (s *recordingManagerEvidenceSink) Emit(_ context.Context, record runnerevidence.Record) error {
	s.mu.Lock()
	s.records = append(s.records, record)
	s.mu.Unlock()
	return nil
}

func (s *recordingManagerEvidenceSink) snapshot() []runnerevidence.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runnerevidence.Record(nil), s.records...)
}

func TestManagerNetworkFailureAndTeardownEvidenceUseLaunchCorrelation(t *testing.T) {
	sink := &recordingManagerEvidenceSink{}
	inst := &instance{
		id:                "fc-instance",
		sandboxID:         "sandbox-1",
		sandboxGeneration: 9,
		compartmentID:     "instance-1",
		requestID:         "request-1",
		operationID:       "operation-1",
		leaseID:           "lease-1",
		assignmentID:      "assignment-1",
		done:              make(chan struct{}),
	}
	manager := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*instance{inst.id: inst},
		guestIPs:  map[string]string{},
		evidence:  sink,
		runnerID:  "runner-1",
	}
	manager.handleNetworkPolicyFailure(inst.id, errors.New("simulated enforcement loss"))
	records := sink.snapshot()
	if len(records) < 2 ||
		records[0].Event != runnerevidence.EventNetworkFailure ||
		records[len(records)-1].Event != runnerevidence.EventTeardownTerminal {
		t.Fatalf("network/teardown evidence = %+v", records)
	}
	for _, record := range records {
		if record.RequestID != "request-1" ||
			record.OperationID != "operation-1" ||
			record.SandboxID != "sandbox-1" ||
			record.InstanceID != "instance-1" ||
			record.SandboxGeneration != 9 ||
			record.AssignmentID != "assignment-1" ||
			record.LeaseID != "lease-1" ||
			record.RunnerID != "runner-1" {
			t.Fatalf("evidence correlation = %+v", record)
		}
	}
}
