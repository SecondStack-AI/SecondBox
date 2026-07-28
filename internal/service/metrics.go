package service

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Metrics holds fixed-cardinality Sandbox domain counters and gauges.
type Metrics struct {
	environmentsCreated atomic.Uint64
	environmentsReused  atomic.Uint64
	startsReady         atomic.Uint64
	startsReused        atomic.Uint64
	stopsCompleted      atomic.Uint64
	instancesLost       atomic.Uint64
	prepareFailures     atomic.Uint64
	startFailures       atomic.Uint64
	publishFailures     atomic.Uint64
	retainedWorkspaces  atomic.Int64
	workspacesPurged    atomic.Uint64
	artifactsExchanged  atomic.Uint64
	artifactBytes       atomic.Uint64
}

// PrometheusText renders bounded service metrics without provider or tenant dimensions.
func (m *Metrics) PrometheusText() string {
	var output strings.Builder
	writeCounter := func(help, metric string, value uint64) {
		fmt.Fprintf(&output, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", metric, help, metric, metric, value)
	}
	writeCounter("Durable Sandbox Environments created.", "sandbox_environments_created_total", m.environmentsCreated.Load())
	writeCounter("Existing Sandbox Environments returned by resolve.", "sandbox_environments_reused_total", m.environmentsReused.Load())
	writeCounter("Sandbox Instance generations made ready.", "sandbox_instance_starts_ready_total", m.startsReady.Load())
	writeCounter("Ready Sandbox Instance generations reused by start.", "sandbox_instance_starts_reused_total", m.startsReused.Load())
	writeCounter("Sandbox Instance generations stopped and destroyed.", "sandbox_instance_stops_completed_total", m.stopsCompleted.Load())
	writeCounter("Sandbox Instance generations reported lost.", "sandbox_instance_lost_total", m.instancesLost.Load())
	writeCounter("Sandbox Instance preparation failures.", "sandbox_instance_prepare_failures_total", m.prepareFailures.Load())
	writeCounter("Sandbox Instance start failures.", "sandbox_instance_start_failures_total", m.startFailures.Load())
	writeCounter("Sandbox Instance readiness publication failures.", "sandbox_instance_publish_failures_total", m.publishFailures.Load())
	fmt.Fprintf(&output, "# HELP sandbox_retained_workspaces Current durable workspaces retained by Sandbox Service.\n")
	fmt.Fprintf(&output, "# TYPE sandbox_retained_workspaces gauge\nsandbox_retained_workspaces %d\n", m.retainedWorkspaces.Load())
	writeCounter("Durable Sandbox workspaces purged after retention.", "sandbox_workspaces_purged_total", m.workspacesPurged.Load())
	writeCounter("Immutable artifacts exchanged from Sandbox Environments.", "sandbox_artifacts_exchanged_total", m.artifactsExchanged.Load())
	writeCounter("Artifact bytes exchanged from Sandbox Environments.", "sandbox_artifact_bytes_exchanged_total", m.artifactBytes.Load())
	return output.String()
}

func (m *Metrics) recordResolve(created bool) {
	if created {
		m.environmentsCreated.Add(1)
		m.retainedWorkspaces.Add(1)
		return
	}
	m.environmentsReused.Add(1)
}

func (m *Metrics) recordStartFailure(code string) {
	switch code {
	case "workspace_unavailable", "resource_class_unavailable", "prepare_failed":
		m.prepareFailures.Add(1)
	case "start_failed":
		m.startFailures.Add(1)
	default:
		m.publishFailures.Add(1)
	}
}
