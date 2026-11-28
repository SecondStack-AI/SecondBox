package integration_test

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestResourceAccountCreateRaceAndReplay(t *testing.T) {
	ctx := t.Context()
	control, store := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, control)
	_, subject, credential := createProjectAccountAndCredential(t, control, admin, "resource-account-create")
	principal := authenticateCredential(t, control, credential)
	profile := createGrantedProfile(t, control, store, admin, subject, "resource-account-create")
	spec := profile.CurrentRevision.Spec
	spec.Lifecycle.InitialState = "running"
	spec.RequireResourceAccount = true
	_, _, err := control.ReviseProfileAtRevisionIdempotent(ctx, admin, profile.Name, "protect-create", contracts.ReviseProfileRequest{Spec: spec}, profile.Revision)
	if err != nil {
		t.Fatal(err)
	}
	req := contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}}
	if _, _, err := control.CreateSandbox(ctx, principal, "no-account", req); !errors.Is(err, ports.ErrInvalidRequest) {
		t.Fatalf("unattributed protected admission: %v", err)
	}
	account := contracts.ResourceAccount{Key: "human:account-a", Limits: contracts.ResourceAccountLimits{MaxActiveInstances: 1, MaxVCPUCount: contracts.Unlimited, MaxMemoryBytes: contracts.Unlimited}}
	req.ResourceAccount = &account
	type result struct {
		key     string
		sandbox contracts.Sandbox
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for i := range 2 {
		go func() {
			<-start
			key := fmt.Sprintf("account-race-%d", i)
			sandbox, _, err := control.CreateSandbox(ctx, principal, key, req)
			results <- result{key, sandbox, err}
		}()
	}
	close(start)
	var accepted, refused result
	for range 2 {
		r := <-results
		if r.err == nil {
			accepted = r
			continue
		}
		var refusal *ports.ResourceAccountQuotaError
		if !errors.As(r.err, &refusal) || refusal.Refusal != (contracts.ResourceAccountQuotaRefusal{Axis: "maxActiveInstances", AccountKey: account.Key, Used: 1, Requested: 1, Limit: 1}) {
			t.Fatalf("account race: %+v", r)
		}
		refused = r
	}
	if accepted.sandbox.ResourceAccountKey != account.Key || refused.key == "" {
		t.Fatalf("accepted=%+v refused=%+v", accepted, refused)
	}
	usage, err := control.GetResourceAccountUsage(ctx, principal, account.Key)
	if err != nil || usage.ActiveInstances != 1 || usage.VCPUCount != 1 || usage.MemoryBytes != 1<<30 || len(usage.Allocations) != 1 || usage.Allocations[0].SandboxID != accepted.sandbox.ID || usage.ObservedAt.IsZero() {
		t.Fatalf("pending create usage=%+v err=%v", usage, err)
	}
	_, _, otherCredential := createProjectAccountAndCredential(t, control, admin, "unrelated-account-subject")
	other := authenticateCredential(t, control, otherCredential)
	isolated, err := control.GetResourceAccountUsage(ctx, other, account.Key)
	if err != nil || isolated.ActiveInstances != 0 || len(isolated.Allocations) != 0 {
		t.Fatalf("scope leaked %+v %v", isolated, err)
	}
	server := contractServer(t, persistedHTTPHandler(t, control, store))
	defer server.Close()
	response := authenticatedJSONRequest(t, http.MethodGet, server.URL+"/v1/resource-accounts/"+account.Key+"/usage", credential, "", nil)
	var observation contracts.ResourceAccountUsage
	decodeResponseJSON(t, response, &observation)
	if response.StatusCode != http.StatusOK || observation.ActiveInstances != 1 || observation.Allocations[0].SandboxID != accepted.sandbox.ID {
		t.Fatalf("HTTP usage=%+v status=%d", observation, response.StatusCode)
	}
	denied := authenticatedJSONRequest(t, http.MethodPost, server.URL+"/v1/sandboxes", credential, "http-quota-refused", req)
	var problem contracts.Problem
	decodeResponseJSON(t, denied, &problem)
	if denied.StatusCode != http.StatusTooManyRequests || problem.Code != "resource_account_quota_exceeded" || problem.Retryable || problem.RetryAfterMilliseconds != nil || denied.Header.Get("Retry-After") != "" || problem.ResourceAccountQuota == nil || problem.ResourceAccountQuota.AccountKey != account.Key {
		t.Fatalf("unexpected HTTP refusal=%+v", problem)
	}
	metrics, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metricText := readResponse(t, metrics)
	if !strings.Contains(metricText, `secondbox_resource_account_refusals_total{axis="maxActiveInstances"} 1`) || !strings.Contains(metricText, `secondbox_resource_account_allocations{state="creating"}`) || strings.Contains(metricText, account.Key) {
		t.Fatalf("resource-account metrics or cardinality: %s", metricText)
	}
	events, err := store.ListAuditEvents(ctx, principal.TenantRef, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(event contracts.AuditEvent) bool {
		return event.Outcome == "denied" && event.Details["resourceAccountKey"] == account.Key && event.Details["axis"] == "maxActiveInstances"
	}) {
		t.Fatal("account refusal audit missing")
	}
	account.Limits.MaxActiveInstances = 0
	replay, created, err := control.CreateSandbox(ctx, principal, accepted.key, req)
	if err != nil || created || replay.ID != accepted.sandbox.ID {
		t.Fatalf("refreshed replay = %+v %v", replay, err)
	}
	account.Key = "human:account-b"
	if _, _, err := control.CreateSandbox(ctx, principal, accepted.key, req); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("replay transferred account: %v", err)
	}
	account.Limits.MaxActiveInstances = 1
	if _, _, err := control.CreateSandbox(ctx, principal, "independent-account", req); err != nil {
		t.Fatal(err)
	}
	account.Key = "human:account-a"
	account.Limits.MaxActiveInstances = 2
	if _, _, err := control.CreateSandbox(ctx, principal, refused.key, req); err != nil {
		t.Fatalf("refresh after refusal: %v", err)
	}
}

func TestResourceAccountStartPinsFootprintAcrossProfileRevisions(t *testing.T) {
	ctx := t.Context()
	control, store := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, control)
	_, subject, credential := createProjectAccountAndCredential(t, control, admin, "resource-account-start")
	principal := authenticateCredential(t, control, credential)
	profile := createGrantedProfile(t, control, store, admin, subject, "resource-account-start")
	spec := profile.CurrentRevision.Spec
	spec.RequireResourceAccount = true
	spec.Resources.VCPUCount = 3
	protected, _, err := control.ReviseProfileAtRevisionIdempotent(ctx, admin, profile.Name, "protect-start", contracts.ReviseProfileRequest{Spec: spec}, profile.Revision)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, _, err := control.CreateSandbox(ctx, principal, "stopped-create", contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	completeFixtureSandboxCreation(t, sandbox.ID)
	sandbox, err = control.GetSandbox(ctx, principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec.Resources.VCPUCount = 1
	spec.RequireResourceAccount = false
	if _, _, err := control.ReviseProfileAtRevisionIdempotent(ctx, admin, profile.Name, "new-defaults", contracts.ReviseProfileRequest{Spec: spec}, protected.Revision); err != nil {
		t.Fatal(err)
	}
	start := func(key string, revision int64, account *contracts.ResourceAccount) (contracts.Operation, bool, error) {
		return control.StartSandboxOperation(ctx, principal, sandbox.ID, key, revision, contracts.StartSandboxRequest{ResourceAccount: account})
	}
	if _, _, err := start("start-missing-account", sandbox.Revision, nil); !errors.Is(err, ports.ErrInvalidRequest) {
		t.Fatalf("pinned authority missing: %v", err)
	}
	account := contracts.ResourceAccount{Key: "user:first", Limits: contracts.ResourceAccountLimits{MaxActiveInstances: contracts.Unlimited, MaxVCPUCount: 2, MaxMemoryBytes: contracts.Unlimited}}
	var refusal *ports.ResourceAccountQuotaError
	if _, _, err := start("account-start", sandbox.Revision, &account); !errors.As(err, &refusal) || refusal.Refusal.Axis != "maxVcpuCount" || refusal.Refusal.Requested != 3 {
		t.Fatalf("pinned cpu refusal: %v %+v", err, refusal)
	}
	account.Limits.MaxVCPUCount = 3
	account.Limits.MaxMemoryBytes = 0
	if _, _, err := start("account-start", sandbox.Revision, &account); !errors.As(err, &refusal) || refusal.Refusal.Axis != "maxMemoryBytes" || refusal.Refusal.Requested != 1<<30 {
		t.Fatalf("pinned memory refusal: %v %+v", err, refusal)
	}
	account.Limits.MaxMemoryBytes = contracts.Unlimited
	op, replayed, err := start("account-start", sandbox.Revision, &account)
	if err != nil || replayed {
		t.Fatalf("start after refusal: %v", err)
	}
	account.Limits.MaxVCPUCount = 0
	again, replayed, err := start("account-start", sandbox.Revision, &account)
	if err != nil || !replayed || again.ID != op.ID {
		t.Fatalf("start replay: %+v %v", again, err)
	}
	account.Key = "user:second"
	if _, _, err := start("account-start", sandbox.Revision, &account); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("changed identity start: %v", err)
	}
	latest, err := control.GetSandbox(ctx, principal, sandbox.ID)
	if err != nil || latest.ResourceAccountKey != "user:first" {
		t.Fatalf("active account changed: %+v %v", latest, err)
	}
	if _, err := control.StopSandbox(ctx, principal, sandbox.ID, "account-stop", latest.Revision); err != nil {
		t.Fatal(err)
	}
	released, err := control.GetResourceAccountUsage(ctx, principal, "user:first")
	if err != nil || released.ActiveInstances != 0 {
		t.Fatalf("queued start cancellation: %+v %v", released, err)
	}
	pool, err := pgxpool.New(ctx, integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// A failed, unattached generation must reserve its retry before placement.
	if _, err := pool.Exec(ctx, `UPDATE secondbox.sandboxes SET state='failed' WHERE id=$1`, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	latest, err = control.GetSandbox(ctx, principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	account.Limits.MaxVCPUCount = 3
	if _, _, err := start("account-resume", latest.Revision, &account); err != nil {
		t.Fatal(err)
	}
	usage, err := control.GetResourceAccountUsage(ctx, principal, account.Key)
	if err != nil || usage.ActiveInstances != 1 || usage.VCPUCount != 3 {
		t.Fatalf("recovery was not charged %v %+v", err, usage)
	}
	for _, state := range []string{"ready", "stopping", "failed"} {
		if _, err := pool.Exec(ctx, `UPDATE secondbox.sandboxes SET state=$2,desired_state='stopped',current_instance_id='attached-generation' WHERE id=$1`, sandbox.ID, state); err != nil {
			t.Fatal(err)
		}
		held, err := control.GetResourceAccountUsage(ctx, principal, account.Key)
		if err != nil || held.ActiveInstances != 1 {
			t.Fatalf("%s lost attached generation charge: %v %+v", state, err, held)
		}
	}
	// A terminally failed operation releases the Workspace mutation slot.
	// Its Instance can still own the generation until the Runner fences it.
	clearMutation := func() {
		_, err := pool.Exec(ctx, `UPDATE secondbox.workspaces SET mutation_kind='',mutation_state='',mutation_operation_id='',mutation_id='',mutation_effect_id='',mutation_expected_generation=NULL,mutation_target_generation=NULL WHERE sandbox_id=$1`, sandbox.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	clearMutation()
	_, err = pool.Exec(ctx, `INSERT INTO secondbox.instances
 (id,sandbox_id,generation,state,guest_liveness,termination_reason,created_at,updated_at,maximum_duration_at)
 VALUES ('attached-generation',$1,$2,'failed','lost','startup_failed',now(),now(),now()+interval '1 hour')`, sandbox.ID, sandbox.Generation)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := control.GetSandbox(ctx, principal, sandbox.ID)
	if err != nil || failed.ResourceAccountKey != account.Key || failed.Instance == nil || failed.Instance.Generation != failed.Generation || failed.Instance.SandboxID != failed.ID {
		t.Fatalf("attached failed observation: %+v %v", failed, err)
	}
	if _, _, err := start("recover-attached", failed.Revision, nil); err != nil {
		t.Fatalf("retained allocation required fresh policy: %v", err)
	}
	held, err := control.GetResourceAccountUsage(ctx, principal, account.Key)
	if err != nil || held.ActiveInstances != 1 {
		t.Fatalf("attached recovery lost original account: %+v %v", held, err)
	}
	clearMutation()
	if _, err := pool.Exec(ctx, `UPDATE secondbox.sandboxes SET current_instance_id='',desired_state='stopped',revision=revision+1 WHERE id=$1`, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := start("stale-after-detach", failed.Revision, nil); !errors.Is(err, ports.ErrRevisionConflict) {
		t.Fatalf("stale retained allocation after detach: %v", err)
	}
	detached, err := control.GetSandbox(ctx, principal, sandbox.ID)
	if err != nil || detached.Instance != nil || detached.ResourceAccountKey != "" {
		t.Fatalf("detached observation: %+v %v", detached, err)
	}
	if _, _, err := start("fresh-after-detach", detached.Revision, nil); !errors.Is(err, ports.ErrInvalidRequest) {
		t.Fatalf("unattributed positive demand after detach: %v", err)
	}
	released, err = control.GetResourceAccountUsage(ctx, principal, account.Key)
	if err != nil || released.ActiveInstances != 0 {
		t.Fatalf("detached failure: %v %+v", err, released)
	}
}

func TestResourceAccountConcurrentStartsAcrossReplicas(t *testing.T) {
	ctx := t.Context()
	first, db := newControlPlaneFixture(t, generousQuota())
	replicaStore, err := store.NewPostgresControlPlaneStore(ctx, integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer replicaStore.Close()
	second := newControlPlaneService(t, replicaStore, generousQuota())
	admin := fixtureAdmin(t, first)
	_, subject, credential := createProjectAccountAndCredential(t, first, admin, "account-start-race")
	p := authenticateCredential(t, first, credential)
	profile := createGrantedProfile(t, first, db, admin, subject, "account-start-race")
	stopped := func(key string) contracts.Sandbox {
		s, _, err := first.CreateSandbox(ctx, p, key, contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}})
		if err != nil {
			t.Fatal(err)
		}
		completeFixtureSandboxCreation(t, s.ID)
		s, err = first.GetSandbox(ctx, p, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	legacy := stopped("uncharged-legacy-create")
	if _, _, err := first.StartSandboxOperation(ctx, p, legacy.ID, "uncharged-legacy-start", legacy.Revision, contracts.StartSandboxRequest{}); err != nil {
		t.Fatal(err)
	}
	account := &contracts.ResourceAccount{Key: "human:cross-replica", Limits: contracts.ResourceAccountLimits{MaxActiveInstances: 1, MaxVCPUCount: contracts.Unlimited, MaxMemoryBytes: contracts.Unlimited}}
	ready := make(chan struct{})
	results := make(chan error, 2)
	for i, replica := range []*service.ControlPlaneService{first, second} {
		sandbox := stopped(fmt.Sprintf("cross-replica-create-%d", i))
		go func() {
			<-ready
			_, _, err := replica.StartSandboxOperation(ctx, p, sandbox.ID, "cross-replica-start", sandbox.Revision, contracts.StartSandboxRequest{ResourceAccount: account})
			results <- err
		}()
	}
	close(ready)
	var successes, refusals int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ports.ErrResourceAccountQuotaExceeded):
			refusals++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || refusals != 1 {
		t.Fatalf("starts accepted=%d refused=%d", successes, refusals)
	}
	capacity, err := second.GetSubjectCapacity(ctx, p)
	if err != nil || capacity.UnattributedActiveInstancesByProfile[profile.Name] != 1 || capacity.Usage.ActiveInstances != 2 {
		t.Fatalf("legacy/charged starts reported %+v, %v", capacity, err)
	}
}

func TestResourceAccountCurrentProfileProtectsLegacyStoppedSandbox(t *testing.T) {
	ctx := t.Context()
	control, db := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, control)
	_, subject, credential := createProjectAccountAndCredential(t, control, admin, "legacy-protected-start")
	principal := authenticateCredential(t, control, credential)
	profile := createGrantedProfile(t, control, db, admin, subject, "legacy-protected-start")
	legacy, _, err := control.CreateSandbox(ctx, principal, "legacy-create-no-account", contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	completeFixtureSandboxCreation(t, legacy.ID)
	legacy, err = control.GetSandbox(ctx, principal, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec := profile.CurrentRevision.Spec
	spec.RequireResourceAccount = true
	spec.Resources.VCPUCount = 3
	if _, _, err := control.ReviseProfileAtRevisionIdempotent(ctx, admin, profile.Name, "new-live-account-guard", contracts.ReviseProfileRequest{Spec: spec}, profile.Revision); err != nil {
		t.Fatal(err)
	}
	_, _, err = control.StartSandboxOperation(ctx, principal, legacy.ID, "legacy-restart", legacy.Revision, contracts.StartSandboxRequest{})
	if !errors.Is(err, ports.ErrInvalidRequest) {
		t.Fatalf("unattributed start after enabling current guard: %v", err)
	}
	account := contracts.ResourceAccount{Key: "user:legacy", Limits: contracts.ResourceAccountLimits{MaxActiveInstances: 1, MaxVCPUCount: 1, MaxMemoryBytes: contracts.Unlimited}}
	_, _, err = control.StartSandboxOperation(ctx, principal, legacy.ID, "legacy-restart", legacy.Revision, contracts.StartSandboxRequest{ResourceAccount: &account})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := control.GetResourceAccountUsage(ctx, principal, account.Key)
	if err != nil || usage.VCPUCount != 1 || usage.ActiveInstances != 1 {
		t.Fatalf("legacy pinned size: %+v %v", usage, err)
	}
}

func TestResourceAccountCurrentProfileRejectsHeldUnattributedRecovery(t *testing.T) {
	ctx := t.Context()
	control, db := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, control)
	_, subject, credential := createProjectAccountAndCredential(t, control, admin, "held-legacy-account")
	principal := authenticateCredential(t, control, credential)
	profile := createGrantedProfile(t, control, db, admin, subject, "held-legacy-account")
	legacy, _, err := control.CreateSandbox(ctx, principal, "held-legacy-create", contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	completeFixtureSandboxCreation(t, legacy.ID)
	pool, err := pgxpool.New(ctx, integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `UPDATE secondbox.sandboxes SET state='failed',current_instance_id='legacy-attached',revision=revision+1 WHERE id=$1`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	spec := profile.CurrentRevision.Spec
	spec.RequireResourceAccount = true
	if _, _, err := control.ReviseProfileAtRevisionIdempotent(ctx, admin, profile.Name, "held-legacy-protection", contracts.ReviseProfileRequest{Spec: spec}, profile.Revision); err != nil {
		t.Fatal(err)
	}
	legacy, err = control.GetSandbox(ctx, principal, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	account := contracts.ResourceAccount{Key: "user:held-legacy", Limits: contracts.ResourceAccountLimits{MaxActiveInstances: 1, MaxVCPUCount: contracts.Unlimited, MaxMemoryBytes: contracts.Unlimited}}
	for _, candidate := range []*contracts.ResourceAccount{nil, &account} {
		if _, _, err := control.StartSandboxOperation(ctx, principal, legacy.ID, "held-legacy-start", legacy.Revision, contracts.StartSandboxRequest{ResourceAccount: candidate}); !errors.Is(err, ports.ErrInvalidRequest) {
			t.Fatalf("held unattributed recovery was admitted: %v", err)
		}
	}
	unchanged, err := control.GetSandbox(ctx, principal, legacy.ID)
	if err != nil || unchanged.Revision != legacy.Revision || unchanged.DesiredState != legacy.DesiredState {
		t.Fatalf("refused recovery changed intent: %+v %v", unchanged, err)
	}
	if _, err := control.StopSandbox(ctx, principal, legacy.ID, "held-legacy-stop", legacy.Revision); err != nil {
		t.Fatal(err)
	}
	// Runner fencing releases the old allocation before a new account can own compute.
	if _, err := pool.Exec(ctx, `UPDATE secondbox.sandboxes SET state='stopped',current_instance_id='',revision=revision+1 WHERE id=$1`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	legacy, err = control.GetSandbox(ctx, principal, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := control.StartSandboxOperation(ctx, principal, legacy.ID, "held-legacy-attributed-start", legacy.Revision, contracts.StartSandboxRequest{ResourceAccount: &account}); err != nil {
		t.Fatalf("released legacy allocation did not adopt account: %v", err)
	}
	usage, err := control.GetResourceAccountUsage(ctx, principal, account.Key)
	if err != nil || usage.ActiveInstances != 1 {
		t.Fatalf("adopted allocation was not charged: %+v %v", usage, err)
	}
}

func TestResourceAccountPlacementCannotResurrectReleasedCharge(t *testing.T) {
	fixture := newSchedulerLockFixture(t)
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.sandboxes SET state='failed',resource_account_key='user:released' WHERE id=$1`, fixture.request.SandboxID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.scheduler.Schedule(t.Context(), fixture.request); !errors.Is(err, ports.ErrRevisionConflict) {
		t.Fatalf("stale plan reacquired an account allocation: %v", err)
	}
}
