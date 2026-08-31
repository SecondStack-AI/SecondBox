package resourceapply

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

type fakeClient struct {
	pools        map[string]secondboxclient.RunnerPool
	profiles     map[string]secondboxclient.Profile
	events       []string
	failRevision int64
	failed       bool
}

func newFakeClient() *fakeClient {
	return &fakeClient{pools: map[string]secondboxclient.RunnerPool{}, profiles: map[string]secondboxclient.Profile{}}
}
func notFound() error { return &secondboxclient.APIError{StatusCode: http.StatusNotFound} }

func (fake *fakeClient) GetRunnerPool(_ context.Context, name string) (secondboxclient.RunnerPool, error) {
	pool, ok := fake.pools[name]
	if !ok {
		return secondboxclient.RunnerPool{}, notFound()
	}
	return pool, nil
}
func (fake *fakeClient) CreateRunnerPool(_ context.Context, request secondboxclient.CreateRunnerPoolRequest) (secondboxclient.RunnerPool, error) {
	fake.events = append(fake.events, "create-pool:"+request.Name)
	if _, ok := fake.pools[request.Name]; ok {
		return secondboxclient.RunnerPool{}, errors.New("race")
	}
	pool := secondboxclient.RunnerPool{Name: request.Name, Architectures: request.Architectures, Capabilities: request.Capabilities, CapacityPolicy: request.CapacityPolicy, State: request.State, Revision: 1}
	fake.pools[request.Name] = pool
	return pool, nil
}
func (fake *fakeClient) UpdateRunnerPool(_ context.Context, name string, expected int64, request secondboxclient.UpdateRunnerPoolRequest) (secondboxclient.RunnerPool, error) {
	pool := fake.pools[name]
	if pool.Revision != expected {
		return secondboxclient.RunnerPool{}, errors.New("revision race")
	}
	fake.events = append(fake.events, "update-pool:"+name)
	if request.State != nil {
		pool.State = *request.State
	}
	if request.CapacityPolicy != nil {
		pool.CapacityPolicy = request.CapacityPolicy
	}
	pool.Revision++
	fake.pools[name] = pool
	return pool, nil
}
func (fake *fakeClient) GetProfile(_ context.Context, name string) (secondboxclient.Profile, error) {
	profile, ok := fake.profiles[name]
	if !ok {
		return secondboxclient.Profile{}, notFound()
	}
	return profile, nil
}
func (fake *fakeClient) CreateProfile(_ context.Context, request secondboxclient.CreateProfileRequest, _ string) (secondboxclient.Profile, error) {
	fake.events = append(fake.events, "create-profile:"+request.Name)
	revision := contracts.ProfileRevision{ID: "revision-1", Number: 1, Spec: request.Spec, CreatedAt: time.Unix(1, 0)}
	profile := secondboxclient.Profile{Name: request.Name, State: secondboxclient.ProfileStateEnabled, CurrentRevision: revision, Revisions: []contracts.ProfileRevision{revision}, Revision: 1}
	fake.profiles[request.Name] = profile
	return profile, nil
}
func (fake *fakeClient) ReviseProfile(_ context.Context, name string, expected int64, request secondboxclient.ReviseProfileRequest, _ string) (secondboxclient.Profile, error) {
	profile := fake.profiles[name]
	number := profile.CurrentRevision.Number + 1
	if fake.failRevision == number && !fake.failed {
		fake.failed = true
		return secondboxclient.Profile{}, errors.New("injected partial failure")
	}
	if profile.Revision != expected {
		return secondboxclient.Profile{}, errors.New("revision race")
	}
	fake.events = append(fake.events, fmt.Sprintf("revise-profile:%s:%d", name, number))
	revision := contracts.ProfileRevision{ID: fmt.Sprintf("revision-%d", number), Number: number, Spec: request.Spec, CreatedAt: time.Unix(number, 0)}
	profile.CurrentRevision = revision
	profile.Revisions = append(profile.Revisions, revision)
	profile.Revision++
	fake.profiles[name] = profile
	return profile, nil
}

func testSpec(pool string, cpu int64) secondboxclient.ProfileRevisionSpec {
	return secondboxclient.ProfileRevisionSpec{Pool: pool, Architecture: "amd64", RuntimeBundleDigest: "sha256:" + repeat("a", 64), ToolchainBundleDigest: "sha256:" + repeat("b", 64), Resources: secondboxclient.ResourcePolicy{VCPUCount: cpu, MemoryBytes: 1 << 30, WorkspaceBytes: 2 << 30, ConcurrentOperations: 4}, Startup: secondboxclient.StartupPolicy{Mode: secondboxclient.StartupModeColdBoot}, Lifecycle: secondboxclient.LifecyclePolicy{InitialState: "running", DrainGraceSeconds: 10, IdleSeconds: 60, MaximumDurationSeconds: 900, LeaseSeconds: 60}, Retention: secondboxclient.RetentionPolicy{SnapshotLimit: 0, SnapshotRetentionSeconds: 1}, Execution: secondboxclient.ExecutionPolicy{MaximumDeadlineMilliseconds: 1, MaximumBufferedOutputBytes: 1, StreamWindowBytes: 1, MaximumTransferBytes: 1, DataPlaneTransport: "proxied"}, Network: secondboxclient.NetworkPolicy{Mode: "deny_all", Destinations: []secondboxclient.NetworkDestination{}, RequiresTenantEgressContext: new(bool)}, Ports: []secondboxclient.PortPolicy{}}
}
func repeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}
func desiredDocument(t *testing.T, revisions int) Document {
	t.Helper()
	lineage := make([]ProfileRevision, 0, revisions)
	for number := 1; number <= revisions; number++ {
		spec := testSpec("pool", int64(number*1000))
		digest, err := SpecDigest(spec)
		if err != nil {
			t.Fatal(err)
		}
		lineage = append(lineage, ProfileRevision{Number: int64(number), SpecDigest: digest, Spec: spec})
	}
	return Document{SchemaVersion: SchemaVersion, RunnerPools: []RunnerPool{{Name: "pool", Architectures: []string{"amd64"}, Capabilities: []string{"local-workspace"}, CapacityPolicy: map[string]int64{"maxSandboxes": 2}, State: "ready", MutableFields: []string{"capacityPolicy", "state"}}}, Profiles: []Profile{{Name: "profile", Revisions: lineage}}}
}

func TestApplyCreatesPoolsBeforeSequentialProfileLineageAndReplaysExactly(t *testing.T) {
	fake := newFakeClient()
	document := desiredDocument(t, 3)
	report, err := Apply(t.Context(), fake, document)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{"create-pool:pool", "create-profile:profile", "revise-profile:profile:2", "revise-profile:profile:3"}
	if !reflect.DeepEqual(fake.events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", fake.events, wantEvents)
	}
	if len(report.Results) != 4 {
		t.Fatalf("results = %#v", report.Results)
	}
	fake.events = nil
	replayed, err := Apply(t.Context(), fake, document)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.events) != 0 || replayed.Results[0].Action != ActionNoop || replayed.Results[1].Action != ActionNoop {
		t.Fatalf("replay = %#v events=%#v", replayed, fake.events)
	}
}

func TestCheckReportsWithoutMutationAndApplyUpdatesOnlyDeclaredPoolFields(t *testing.T) {
	fake := newFakeClient()
	document := desiredDocument(t, 1)
	fake.pools["pool"] = secondboxclient.RunnerPool{Name: "pool", Architectures: []string{"amd64"}, Capabilities: []string{"local-workspace"}, CapacityPolicy: map[string]int64{"maxSandboxes": 1}, State: "draining", Revision: 7}
	report, err := Check(t.Context(), fake, document)
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied || report.Results[0].Action != ActionUpdate || len(fake.events) != 0 {
		t.Fatalf("check = %#v events=%#v", report, fake.events)
	}
	if _, err := Apply(t.Context(), fake, document); err != nil {
		t.Fatal(err)
	}
	if fake.pools["pool"].Revision != 8 || fake.pools["pool"].State != "ready" {
		t.Fatalf("updated pool = %#v", fake.pools["pool"])
	}
}

func TestApplyTreatsRunnerPoolArchitecturesAndCapabilitiesAsSets(t *testing.T) {
	fake := newFakeClient()
	document := desiredDocument(t, 1)
	document.RunnerPools[0].Architectures = []string{"amd64", "arm64"}
	document.RunnerPools[0].Capabilities = []string{"local-workspace", "exec-streaming"}
	if _, err := Apply(t.Context(), fake, document); err != nil {
		t.Fatal(err)
	}
	pool := fake.pools["pool"]
	pool.Architectures = []string{"arm64", "amd64"}
	pool.Capabilities = []string{"exec-streaming", "local-workspace"}
	fake.pools["pool"] = pool
	fake.events = nil
	replayed, err := Apply(t.Context(), fake, document)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.events) != 0 || replayed.Results[0].Action != ActionNoop {
		t.Fatalf("replay = %#v events=%#v", replayed, fake.events)
	}
	pool.Capabilities = []string{"exec-streaming"}
	fake.pools["pool"] = pool
	if _, err := Apply(t.Context(), fake, document); err == nil {
		t.Fatal("expected capability drift failure")
	}
}

func TestApplyRejectsHistoricalDriftFutureHeadsGapsAndRevisionRaces(t *testing.T) {
	document := desiredDocument(t, 2)
	for _, test := range []struct {
		name   string
		mutate func(*fakeClient)
	}{
		{"altered history", func(fake *fakeClient) { fake.profiles["profile"].Revisions[0].Spec.Resources.VCPUCount++ }},
		{"future head", func(fake *fakeClient) {
			profile := fake.profiles["profile"]
			extra := profile.CurrentRevision
			extra.Number = 3
			profile.Revisions = append(profile.Revisions, extra)
			profile.CurrentRevision = extra
			fake.profiles["profile"] = profile
		}},
		{"gap", func(fake *fakeClient) {
			profile := fake.profiles["profile"]
			profile.Revisions[0].Number = 2
			fake.profiles["profile"] = profile
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeClient()
			if _, err := Apply(t.Context(), fake, desiredDocument(t, 1)); err != nil {
				t.Fatal(err)
			}
			test.mutate(fake)
			if _, err := Check(t.Context(), fake, document); err == nil {
				t.Fatal("expected drift failure")
			}
		})
	}
	fake := newFakeClient()
	if _, err := Apply(t.Context(), fake, desiredDocument(t, 1)); err != nil {
		t.Fatal(err)
	}
	pool := fake.pools["pool"]
	pool.CapacityPolicy = map[string]int64{"maxSandboxes": 1}
	pool.Revision = 4
	fake.pools["pool"] = pool
	racing := &raceClient{fakeClient: fake}
	if _, err := Apply(t.Context(), racing, document); err == nil {
		t.Fatal("expected optimistic race")
	}
}

type raceClient struct{ *fakeClient }

func (race *raceClient) UpdateRunnerPool(ctx context.Context, name string, expected int64, request secondboxclient.UpdateRunnerPoolRequest) (secondboxclient.RunnerPool, error) {
	pool := race.pools[name]
	pool.Revision++
	race.pools[name] = pool
	return race.fakeClient.UpdateRunnerPool(ctx, name, expected, request)
}

func TestInterruptedApplicationRetriesFromTheInstalledPrefix(t *testing.T) {
	fake := newFakeClient()
	fake.failRevision = 2
	document := desiredDocument(t, 3)
	if _, err := Apply(t.Context(), fake, document); err == nil {
		t.Fatal("expected injected failure")
	}
	if fake.profiles["profile"].CurrentRevision.Number != 1 {
		t.Fatalf("partial head = %d", fake.profiles["profile"].CurrentRevision.Number)
	}
	if _, err := Apply(t.Context(), fake, document); err != nil {
		t.Fatal(err)
	}
	if fake.profiles["profile"].CurrentRevision.Number != 3 {
		t.Fatalf("converged head = %d", fake.profiles["profile"].CurrentRevision.Number)
	}
}

func TestDocumentValidationRejectsUnknownFieldsGapsBadDigestsAndOmissionDoesNotDelete(t *testing.T) {
	if _, err := Decode([]byte(`{"schemaVersion":"secondbox.resources/v1","runnerPools":[],"profiles":[],"extra":true}`)); err == nil {
		t.Fatal("expected unknown field rejection")
	}
	document := desiredDocument(t, 2)
	document.Profiles[0].Revisions[1].Number = 3
	if err := document.Validate(); err == nil {
		t.Fatal("expected gap rejection")
	}
	document = desiredDocument(t, 1)
	document.Profiles[0].Revisions[0].SpecDigest = "sha256:" + repeat("0", 64)
	if err := document.Validate(); err == nil {
		t.Fatal("expected digest rejection")
	}
	fake := newFakeClient()
	fake.pools["unmanaged"] = secondboxclient.RunnerPool{Name: "unmanaged", Revision: 1}
	if _, err := Apply(t.Context(), fake, desiredDocument(t, 1)); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.pools["unmanaged"]; !ok {
		t.Fatal("omitted resource was deleted")
	}
}
