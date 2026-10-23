package integration_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// TestQuotaRefusalsDistinguishComputeFromDurableAxes covers the refusal class
// the HTTP layer turns into a retry hint. A start or create refused on active
// Instances frees when another Instance stops, so it is a compute refusal; a
// create refused on the Sandbox count frees only when a Sandbox is deleted.
func TestQuotaRefusalsDistinguishComputeFromDurableAxes(t *testing.T) {
	fixture := newTeardownFixture(t)
	first := createStoppedQuotaSandbox(t, fixture, "first")
	second := createStoppedQuotaSandbox(t, fixture, "second")
	setFixtureSubjectQuota(t, fixture, "max_active_instances", 1)
	if _, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, first.ID, "compute-quota-first-"+first.ID,
		first.Revision, testExecutionImage(),
	); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, second.ID, "compute-quota-second-"+second.ID,
		second.Revision, testExecutionImage(),
	)
	if !errors.Is(err, ports.ErrComputeQuotaExceeded) {
		t.Fatalf("start over the active-Instance quota error = %v, want the compute quota refusal", err)
	}

	// The same axis exhausted at Tenant scope is still a compute refusal.
	setFixtureSubjectQuota(t, fixture, "max_active_instances", 10)
	tag, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.tenant_quotas SET max_active_instances=1 WHERE tenant_ref=$1`, fixture.tenantRef,
	)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("Tenant quota update changed %d rows, error %v", tag.RowsAffected(), err)
	}
	_, err = fixture.controlPlane.StartSandbox(
		t.Context(), fixture.principal, second.ID, "compute-quota-tenant-"+second.ID,
		second.Revision, testExecutionImage(),
	)
	if !errors.Is(err, ports.ErrComputeQuotaExceeded) {
		t.Fatalf("start over the Tenant active-Instance quota error = %v, want the compute quota refusal", err)
	}

	// A create whose Profile starts the Sandbox reserves an Instance, so it is
	// refused on compute; with the Sandbox count exhausted as well, the durable
	// refusal wins and carries no retry hint.
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.tenant_quotas SET max_active_instances=10 WHERE tenant_ref=$1`, fixture.tenantRef,
	); err != nil {
		t.Fatal(err)
	}
	setFixtureSubjectQuota(t, fixture, "max_active_instances", 1)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.profile_revisions AS revision
		SET spec_json=jsonb_set(revision.spec_json,'{lifecycle,initialState}','"running"')
		FROM secondbox.profiles AS profile
		WHERE profile.name=$1 AND revision.id=profile.current_revision_id`, fixture.profileName,
	); err != nil {
		t.Fatal(err)
	}
	createRunning := func(label string) error {
		_, _, err := fixture.controlPlane.CreateSandboxOperation(
			t.Context(), fixture.principal,
			label+"-"+strconv.FormatInt(integrationIdentitySequence.Add(1), 10),
			contracts.CreateSandboxRequest{Image: testExecutionImage(),
				Profile: fixture.profileName, Metadata: map[string]string{},
			},
		)
		return err
	}
	if err := createRunning("compute-quota-create"); !errors.Is(err, ports.ErrComputeQuotaExceeded) {
		t.Fatalf("running-initial create over the active-Instance quota error = %v, want the compute quota refusal", err)
	}
	setFixtureSubjectQuota(t, fixture, "max_sandboxes", 2)
	if err := createRunning("durable-quota-create"); !errors.Is(err, ports.ErrQuotaExceeded) ||
		errors.Is(err, ports.ErrComputeQuotaExceeded) {
		t.Fatalf("create over both the Sandbox and active-Instance quotas error = %v, want the durable refusal", err)
	}
}

func createStoppedQuotaSandbox(t *testing.T, fixture *teardownFixture, label string) contracts.Sandbox {
	t.Helper()
	operation, created, err := fixture.controlPlane.CreateSandboxOperation(
		t.Context(), fixture.principal,
		"quota-create-"+label+"-"+strconv.FormatInt(integrationIdentitySequence.Add(1), 10),
		contracts.CreateSandboxRequest{Image: testExecutionImage(),
			Profile: fixture.profileName, Metadata: map[string]string{},
		},
	)
	if err != nil || !created {
		t.Fatalf("create Sandbox created=%t error=%v", created, err)
	}
	fixture.completeWorkspaceCreate(t, operation.SandboxID, operation.ID)
	sandbox, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, operation.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	return sandbox
}

// setFixtureSubjectQuota lowers one Subject quota limit for the fixture's own
// Subject, which no other test shares.
func setFixtureSubjectQuota(t *testing.T, fixture *teardownFixture, column string, limit int64) {
	t.Helper()
	switch column {
	case "max_active_instances", "max_sandboxes":
	default:
		t.Fatalf("Subject quota column %q is not settable here", column)
	}
	tag, err := fixture.pool.Exec(t.Context(), `
		UPDATE secondbox.subject_quotas SET `+column+`=$3,updated_at=$4
		WHERE tenant_ref=$1 AND subject_ref=$2`,
		fixture.tenantRef, fixture.subjectRef, limit, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("Subject quota update changed %d rows", tag.RowsAffected())
	}
}
