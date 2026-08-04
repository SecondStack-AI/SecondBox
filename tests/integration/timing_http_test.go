package integration_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestTimingHTTPReadsPersistedStageEvidenceAndCurrentAPILatency(t *testing.T) {
	databaseStore, err := store.NewPostgresControlPlaneStore(
		t.Context(), integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	controlPlane, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store:                 databaseStore,
		PlatformToken:         testPlatformToken,
		DefaultSubjectQuota:   generousQuota(),
		Now:                   service.SystemClock,
		NewID:                 service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	base := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := pool.Exec(
		t.Context(),
		`
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			'sbox_timing_http','tenant-timing-http','subject-timing-http',
			'profile','profile-revision','ready','running',1,'workspace-timing-http',
			'instance-timing-http','{}','{}',1,$1,$1
		);
		INSERT INTO secondbox.operations (
			id,tenant_ref,subject_ref,sandbox_id,snapshot_id,kind,state,request_id,
			request_metadata_json,error_code,error_message,retryable,
			created_at,started_at,completed_at,updated_at
		) VALUES (
			'op_timing_http','tenant-timing-http','subject-timing-http',
			'sbox_timing_http','','create','succeeded','request-timing-http',
			'{}','','',false,$1,$2,$3,$3
		);
		INSERT INTO secondbox.assignments (
			id,sandbox_id,instance_id,runner_id,profile_revision_id,backend_kind,
			backend_reference,generation,fencing_token,state,capability_snapshot_json,
			resolved_artifacts_json,release_proof_json,failure_class,retry_count,retry_limit,
			operation_deadline,claim_expires_at,reconcile_owner,reconcile_claim_expires_at,
			next_reconcile_at,revision,created_at,updated_at
		) VALUES (
			'assignment-timing-http','sbox_timing_http','instance-timing-http',
			'runner-timing-http','profile-revision','compute','',1,$4,'ready',
			'{}','[]','{}','',0,3,$5,$5,'',$1,$5,1,$1,$3
		);
		INSERT INTO secondbox.assignment_stage_timings (
			assignment_id,operation_id,sandbox_id,stage,observed_at,received_at
		) VALUES
			('assignment-timing-http','op_timing_http','sbox_timing_http',
			 'artifact_verify',$6,$6),
			('assignment-timing-http','op_timing_http','sbox_timing_http',
			 'compute_launch',$7,$7),
			('assignment-timing-http','op_timing_http','sbox_timing_http',
			 'ready',$8,$8)`,
		pgx.QueryExecModeSimpleProtocol,
		base,
		base.Add(10*time.Millisecond),
		base.Add(120*time.Millisecond),
		[]byte("01234567890123456789012345678901"),
		base.Add(time.Hour),
		base.Add(20*time.Millisecond),
		base.Add(80*time.Millisecond),
		base.Add(100*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken,
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)

	health, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusAndClose(t, health, http.StatusOK)

	sandboxResponse := timingHTTPRequest(
		t, server.URL+"/v1/sandboxes/sbox_timing_http/timings?limit=5",
		"tenant-timing-http", "subject-timing-http",
	)
	if sandboxResponse.StatusCode != http.StatusOK {
		t.Fatalf(
			"Sandbox timing status=%d body=%s",
			sandboxResponse.StatusCode, readResponse(t, sandboxResponse),
		)
	}
	var sandboxTiming contracts.SandboxTiming
	decodeResponseJSON(t, sandboxResponse, &sandboxTiming)
	if len(sandboxTiming.Operations) != 1 ||
		len(sandboxTiming.Operations[0].Boots) != 1 ||
		len(sandboxTiming.Operations[0].Boots[0].Stages) != 3 ||
		sandboxTiming.Operations[0].Boots[0].Stages[1].Stage != "compute_launch" ||
		sandboxTiming.Operations[0].Boots[0].Stages[1].ElapsedMilliseconds != 60 {
		t.Fatalf("Sandbox timing = %#v", sandboxTiming)
	}

	operationResponse := timingHTTPRequest(
		t, server.URL+"/v1/operations/op_timing_http/timings",
		"tenant-timing-http", "subject-timing-http",
	)
	if operationResponse.StatusCode != http.StatusOK {
		t.Fatalf(
			"Operation timing status=%d body=%s",
			operationResponse.StatusCode, readResponse(t, operationResponse),
		)
	}
	var operationTiming contracts.OperationTiming
	decodeResponseJSON(t, operationResponse, &operationTiming)
	if operationTiming.QueueMilliseconds == nil ||
		*operationTiming.QueueMilliseconds != 10 ||
		operationTiming.ExecutionMilliseconds == nil ||
		*operationTiming.ExecutionMilliseconds != 110 {
		t.Fatalf("Operation timing = %#v", operationTiming)
	}

	summaryResponse := timingHTTPRequest(
		t, server.URL+"/v1/timings?windowSeconds=3600",
		"secondbox", "secondbox-admin",
	)
	if summaryResponse.StatusCode != http.StatusOK {
		t.Fatalf(
			"deployment timing status=%d body=%s",
			summaryResponse.StatusCode, readResponse(t, summaryResponse),
		)
	}
	var summary contracts.DeploymentTimingSummary
	decodeResponseJSON(t, summaryResponse, &summary)
	hasComputeLaunch := false
	for _, stage := range summary.BootStages {
		hasComputeLaunch = hasComputeLaunch || stage.Stage == "compute_launch"
	}
	if summary.Boot.Count < 1 || summary.DominantBootStage == nil ||
		!hasComputeLaunch || summary.API.Count < 3 {
		t.Fatalf("deployment timing summary = %#v", summary)
	}

	missingLimit := timingHTTPRequest(
		t, server.URL+"/v1/sandboxes/sbox_timing_http/timings",
		"tenant-timing-http", "subject-timing-http",
	)
	assertHTTPStatusAndClose(t, missingLimit, http.StatusBadRequest)
	isolated := timingHTTPRequest(
		t, server.URL+"/v1/sandboxes/sbox_timing_http/timings?limit=5",
		"other-tenant", "other-subject",
	)
	assertHTTPStatusAndClose(t, isolated, http.StatusNotFound)
}

func timingHTTPRequest(
	t *testing.T,
	endpoint string,
	tenantRef string,
	subjectRef string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, endpoint, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testPlatformToken)
	request.Header.Set("X-SecondBox-Tenant-Ref", tenantRef)
	request.Header.Set("X-SecondBox-Subject-Ref", subjectRef)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
