package integration_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestBuiltInProfilesResolvePinAndRejectOperatorMutation(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	registerBuiltInProfilePool(t, databaseStore)

	profiles := make(map[string]contracts.Profile, 2)
	for _, name := range []string{
		service.BuiltInProfileAgentCompartment,
		service.BuiltInProfileCodingEnvironment,
	} {
		profile, err := controlPlane.GetProfile(t.Context(), admin, name)
		if err != nil {
			t.Fatal(err)
		}
		if profile.Name != name ||
			profile.CurrentRevision.ID == "" ||
			profile.CurrentRevision.Spec.Resources.CPUMillis < 1 ||
			profile.CurrentRevision.Spec.Resources.MemoryBytes < 1 ||
			profile.CurrentRevision.Spec.Resources.WorkspaceBytes < 1 {
			t.Fatalf("built-in Profile %q is incomplete: %#v", name, profile)
		}
		profiles[name] = profile

		if _, err := controlPlane.CreateProfile(
			t.Context(), admin,
			contracts.CreateProfileRequest{Name: name, Spec: profile.CurrentRevision.Spec},
		); err == nil {
			t.Fatalf("operator created reserved built-in Profile %q", name)
		}
		if _, err := controlPlane.ReviseProfile(
			t.Context(), admin, name,
			contracts.ReviseProfileRequest{Spec: profile.CurrentRevision.Spec},
		); err == nil {
			t.Fatalf("operator revised reserved built-in Profile %q", name)
		}
		if _, err := controlPlane.DisableProfile(t.Context(), admin, name); err == nil {
			t.Fatalf("operator disabled reserved built-in Profile %q", name)
		}
	}

	_, _, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "built-in-profile-pinning",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	for index, name := range []string{
		service.BuiltInProfileAgentCompartment,
		service.BuiltInProfileCodingEnvironment,
	} {
		sandbox, _, err := controlPlane.CreateSandbox(
			t.Context(), principal, fmt.Sprintf("builtin-pin-%d", index),
			contracts.CreateSandboxRequest{Profile: name, Metadata: map[string]string{}},
		)
		if err != nil {
			t.Fatal(err)
		}
		if sandbox.ProfileRevisionID != profiles[name].CurrentRevision.ID {
			t.Fatalf(
				"Sandbox on %q pinned %q, want %q",
				name, sandbox.ProfileRevisionID, profiles[name].CurrentRevision.ID,
			)
		}
	}
}

func TestSandboxOnBuiltInSurvivesLaterBuiltInRevision(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	registerBuiltInProfilePool(t, databaseStore)
	agentV1, err := controlPlane.GetProfile(
		t.Context(), admin, service.BuiltInProfileAgentCompartment,
	)
	if err != nil {
		t.Fatal(err)
	}
	codingV1, err := controlPlane.GetProfile(
		t.Context(), admin, service.BuiltInProfileCodingEnvironment,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "built-in-profile-upgrade",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	existing, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "builtin-before-upgrade",
		contracts.CreateSandboxRequest{
			Profile:  service.BuiltInProfileAgentCompartment,
			Metadata: map[string]string{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	agentV2 := agentV1
	agentV2.CurrentRevision.Number = agentV1.CurrentRevision.Number + 1
	agentV2.CurrentRevision.ID = fmt.Sprintf(
		"prv_builtin_agent_compartment_v%d", agentV2.CurrentRevision.Number,
	)
	agentV2.CurrentRevision.Spec.Resources.CPUMillis++
	agentV2.CurrentRevision.CreatedAt = agentV1.CurrentRevision.CreatedAt.Add(24 * time.Hour)
	agentV2.UpdatedAt = agentV2.CurrentRevision.CreatedAt
	upgraded, err := newControlPlaneWithBuiltIns(
		t, databaseStore, []contracts.Profile{agentV2, codingV1},
	)
	if err != nil {
		t.Fatal(err)
	}
	resolvedV2, err := upgraded.GetProfile(
		t.Context(), admin, service.BuiltInProfileAgentCompartment,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedV2.CurrentRevision.ID != agentV2.CurrentRevision.ID {
		t.Fatalf("resolved built-in revision = %q, want %q", resolvedV2.CurrentRevision.ID, agentV2.CurrentRevision.ID)
	}

	stillPinned, err := upgraded.GetSandbox(t.Context(), principal, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillPinned.ProfileRevisionID != agentV1.CurrentRevision.ID {
		t.Fatalf(
			"existing Sandbox built-in revision changed to %q, want %q",
			stillPinned.ProfileRevisionID, agentV1.CurrentRevision.ID,
		)
	}
	future, _, err := upgraded.CreateSandbox(
		t.Context(), principal, "builtin-after-upgrade",
		contracts.CreateSandboxRequest{
			Profile:  service.BuiltInProfileAgentCompartment,
			Metadata: map[string]string{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if future.ProfileRevisionID != agentV2.CurrentRevision.ID {
		t.Fatalf("future Sandbox pinned %q, want %q", future.ProfileRevisionID, agentV2.CurrentRevision.ID)
	}
}

func newControlPlaneWithBuiltIns(
	t *testing.T,
	databaseStore *store.PostgresControlPlaneStore,
	builtIns []contracts.Profile,
) (*service.ControlPlaneService, error) {
	t.Helper()
	return service.NewControlPlaneService(service.ControlPlaneConfig{
		Store:               databaseStore,
		PlatformToken:       testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now: func() time.Time {
			return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
		},
		NewID: newFixtureID,
		NewCredentialMaterial: func() string {
			return fmt.Sprintf("credential-material-%032d", integrationIdentitySequence.Add(1))
		},
		BuiltInProfiles: builtIns,
	})
}

func registerBuiltInProfilePool(
	t *testing.T,
	databaseStore *store.PostgresControlPlaneStore,
) {
	t.Helper()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: "default-pool", State: contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"},
		Capabilities:  []string{"firecracker", "checkpoint"},
		CapacityPolicy: map[string]int64{
			"maxInstances": 100,
		},
		ReadyRunnerCount: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
}
