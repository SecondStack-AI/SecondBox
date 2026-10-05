package integration_test

import (
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// TestDataPlaneAdmissionEvaluatesTheSwitchedProfileGrant proves that admission
// checks the Profile it locks rather than one read before a switch.
func TestDataPlaneAdmissionEvaluatesTheSwitchedProfileGrant(t *testing.T) {
	fixture := newProfileSwitchFixture(t, "switch-admission")
	online := fixture.createProfile(t, "profile-switch-admission-online", fixture.onlineWorkspaceSpec())
	offline := fixture.createProfile(t, "profile-switch-admission-offline", fixture.offlineWorkspaceSpec())
	sandbox := fixture.createReadySandbox(t, online.Name, "switch-admission-create")
	sandbox, _, err := fixture.switchProfile(
		t.Context(), sandbox.ID, "switch-admission-offline", sandbox.Revision, offline.Name,
	)
	if err != nil {
		t.Fatal(err)
	}
	seedDataPlaneReadyAssignment(t, sandbox, fixture.now)
	lease, err := fixture.controlPlane.AcquireSandboxLease(
		t.Context(), fixture.principal, sandbox.ID, sandbox.Generation, "switch-admission-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	dataPlaneStore, err := runnercontrol.NewPostgresDataPlaneStore(t.Context(), runnercontrol.PostgresDataPlaneStoreConfig{
		DatabaseURL: integrationDatabaseURL, Retention: time.Hour, MaximumSessionBytes: 2 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dataPlaneStore.Close)
	admission := func(grants []string, suffix string) runnercontrol.DataPlaneAdmission {
		return runnercontrol.DataPlaneAdmission{
			ID: "dps_switch_admission_" + suffix, StreamID: "stream_switch_admission_" + suffix,
			TenantRef: fixture.principal.TenantRef, SubjectRef: fixture.principal.SubjectRef,
			SandboxID: sandbox.ID, LeaseID: lease.ID, Generation: sandbox.Generation,
			RequestID: "request-switch-admission-" + suffix, Kind: "exec", Operation: "exec",
			IdempotencyKey: "switch-admission-" + suffix, RequestHash: "switch-admission-hash-" + suffix,
			DeadlineAt: fixture.now.Add(time.Minute), MaximumResponseBytes: 1024,
			ExecOpen: &runnerv1.ExecOpen{
				Command:        &runnerv1.ExecOpen_Shell{Shell: "true"},
				DeadlineUnixMs: uint64(fixture.now.Add(time.Minute).UnixMilli()), OutputLimitBytes: 1024,
			},
			ProfileGrants: grants, Now: fixture.now,
		}
	}
	if _, _, err := dataPlaneStore.AdmitDataPlane(
		t.Context(), admission([]string{online.Name}, "left"),
	); !errors.Is(err, ports.ErrAuthorizationDenied) {
		t.Fatalf("exec admission with only the left Profile grant error = %v", err)
	}
	if _, _, err := dataPlaneStore.AdmitDataPlane(
		t.Context(), admission([]string{offline.Name}, "current"),
	); err != nil {
		t.Fatalf("exec admission with the current Profile grant error = %v", err)
	}

	portService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: fixture.databaseStore, PlatformToken: testPlatformToken,
		Now: func() time.Time { return fixture.now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneStore:        dataPlaneStore, DataPlanePollInterval: time.Millisecond,
		PortSessionStore: dataPlaneStore, PublicBaseURL: "https://secondbox.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	createPort := func(grants []string, key string) error {
		_, _, err := portService.CreateSandboxPortSession(
			service.ContextWithApplicationProfileGrants(t.Context(), grants),
			fixture.principal, "request-"+key, sandbox.ID, sandbox.Generation, lease.ID, key,
			contracts.PortTransportProxied, contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 30},
		)
		return err
	}
	if err := createPort([]string{online.Name}, "switch-admission-port-left"); !errors.Is(err, ports.ErrAuthorizationDenied) {
		t.Fatalf("Port admission with only the left Profile grant error = %v", err)
	}
	if err := createPort([]string{offline.Name}, "switch-admission-port-current"); err != nil {
		t.Fatalf("Port admission with the current Profile grant error = %v", err)
	}
}
