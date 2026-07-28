package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/tests/compatibility/initialv1client"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresMigrationAdoptsOnlyTheExactInitialV1Catalog(t *testing.T) {
	if err := postgresmigrations.Apply(t.Context(), integrationDatabaseURL); err != nil {
		t.Fatal(err)
	}
	if err := postgresmigrations.Apply(t.Context(), integrationDatabaseURL); err != nil {
		t.Fatalf("idempotent migration replay failed: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var version, recordedChecksum string
	if err := pool.QueryRow(t.Context(), `
		SELECT version,checksum_sha256
		FROM secondbox.schema_migrations
		ORDER BY version`,
	).Scan(&version, &recordedChecksum); err != nil {
		t.Fatal(err)
	}
	if version != "0001_secondbox" ||
		recordedChecksum != initialV1MigrationSHA256(t) {
		t.Fatalf(
			"adopted migration = %q %q",
			version,
			recordedChecksum,
		)
	}

	t.Run("fresh database", func(t *testing.T) {
		databaseURL := newUpgradeCompatibilityDatabase(t, "fresh")
		const concurrentReplicas = 4
		failures := make(chan error, concurrentReplicas)
		var waitGroup sync.WaitGroup
		for range concurrentReplicas {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				failures <- postgresmigrations.Apply(t.Context(), databaseURL)
			}()
		}
		waitGroup.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Errorf("concurrent fresh migration failed: %v", err)
			}
		}
		if err := postgresmigrations.Apply(t.Context(), databaseURL); err != nil {
			t.Fatalf("fresh migration replay failed: %v", err)
		}
		assertUpgradeCompatibilityMigrationLedger(t, databaseURL)
	})

	t.Run("exact raw baseline", func(t *testing.T) {
		databaseURL := newUpgradeCompatibilityDatabase(t, "raw")
		applyInitialV1RawMigration(t, databaseURL)
		if err := postgresmigrations.Apply(t.Context(), databaseURL); err != nil {
			t.Fatal(err)
		}
		assertUpgradeCompatibilityMigrationLedger(t, databaseURL)
	})

	t.Run("partial schema", func(t *testing.T) {
		databaseURL := newUpgradeCompatibilityDatabase(t, "partial")
		connection, err := pgx.Connect(t.Context(), databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(t.Context(), `
			CREATE SCHEMA secondbox;
			CREATE TABLE secondbox.schema_migrations (
			  version text NOT NULL,
			  checksum_sha256 text NOT NULL,
			  applied_at timestamptz NOT NULL
			);
			CREATE TABLE secondbox.projects (id text PRIMARY KEY);`,
		); err != nil {
			t.Fatal(err)
		}
		if err := connection.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		err = postgresmigrations.Apply(t.Context(), databaseURL)
		if err == nil ||
			!strings.Contains(err.Error(), "untracked baseline catalog mismatch") {
			t.Fatalf("partial schema migration error = %v", err)
		}
	})

	t.Run("checksum drift", func(t *testing.T) {
		databaseURL := newUpgradeCompatibilityDatabase(t, "drift")
		if err := postgresmigrations.Apply(t.Context(), databaseURL); err != nil {
			t.Fatal(err)
		}
		connection, err := pgx.Connect(t.Context(), databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(t.Context(), `
			UPDATE secondbox.schema_migrations
			SET checksum_sha256=$1
			WHERE version='0001_secondbox'`,
			strings.Repeat("0", 64),
		); err != nil {
			t.Fatal(err)
		}
		if err := connection.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		err = postgresmigrations.Apply(t.Context(), databaseURL)
		if err == nil || !strings.Contains(err.Error(), "migration checksum drift") {
			t.Fatalf("checksum drift migration error = %v", err)
		}
	})

	t.Run("ahead ledger", func(t *testing.T) {
		databaseURL := newUpgradeCompatibilityDatabase(t, "ahead")
		if err := postgresmigrations.Apply(t.Context(), databaseURL); err != nil {
			t.Fatal(err)
		}
		connection, err := pgx.Connect(t.Context(), databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(t.Context(), `
			INSERT INTO secondbox.schema_migrations (version,checksum_sha256,applied_at)
			VALUES ('9999_unknown','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',clock_timestamp())`,
		); err != nil {
			t.Fatal(err)
		}
		if err := connection.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		err = postgresmigrations.Apply(t.Context(), databaseURL)
		if err == nil || !strings.Contains(err.Error(), "ledger is ahead of embedded lineage") {
			t.Fatalf("ahead migration ledger error = %v", err)
		}
	})

	t.Run("missing earlier migration", func(t *testing.T) {
		databaseURL := newUpgradeCompatibilityDatabase(t, "missing")
		if err := postgresmigrations.Apply(t.Context(), databaseURL); err != nil {
			t.Fatal(err)
		}
		connection, err := pgx.Connect(t.Context(), databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(t.Context(), `
			DELETE FROM secondbox.schema_migrations;
			INSERT INTO secondbox.schema_migrations (version,checksum_sha256,applied_at)
			VALUES ('0002_gap','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',clock_timestamp())`,
		); err != nil {
			t.Fatal(err)
		}
		if err := connection.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		err = postgresmigrations.Apply(t.Context(), databaseURL)
		if err == nil ||
			!strings.Contains(err.Error(), "ledger is not an embedded prefix") {
			t.Fatalf("missing earlier migration error = %v", err)
		}
	})

	t.Run("untracked schema object", func(t *testing.T) {
		databaseURL := newUpgradeCompatibilityDatabase(t, "untracked")
		applyInitialV1RawMigration(t, databaseURL)
		connection, err := pgx.Connect(t.Context(), databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(t.Context(), `
			CREATE VIEW secondbox.untracked_upgrade_view AS SELECT 1::bigint AS value`,
		); err != nil {
			t.Fatal(err)
		}
		if err := connection.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		err = postgresmigrations.Apply(t.Context(), databaseURL)
		if err == nil ||
			!strings.Contains(err.Error(), "untracked baseline catalog mismatch") {
			t.Fatalf("untracked schema object migration error = %v", err)
		}
	})
}

func TestInitialV1ClientProfileCheckpointAndRollingControlPlaneReplacement(t *testing.T) {
	profileSpec := readInitialV1Fixture[contracts.ProfileRevisionSpec](
		t,
		"initial-v1-profile-revision.json",
	)
	checkpointCompatibility := readInitialV1Fixture[map[string]string](
		t,
		"initial-v1-checkpoint.json",
	)
	controlPlaneA, storeA := newControlPlaneFixture(t, generousQuota())
	admin := controlPlaneA.BootstrapAdmin()
	_, account, credential := createProjectAccountAndCredential(
		t,
		controlPlaneA,
		admin,
		"upgrade-v1",
	)
	if err := storeA.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: "default-pool", State: contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"}, Capabilities: []string{"firecracker", "checkpoint"},
		CapacityPolicy: map[string]int64{"maxInstances": 100}, ReadyRunnerCount: 1,
		Revision: 1, CreatedAt: upgradeCompatibilityNow(), UpdatedAt: upgradeCompatibilityNow(),
	}); err != nil {
		t.Fatal(err)
	}
	profile, err := controlPlaneA.CreateProfile(t.Context(), admin, contracts.CreateProfileRequest{
		Name: "profile-upgrade-v1",
		Spec: profileSpec,
	})
	if err != nil {
		t.Fatal(err)
	}
	grants := append([]string(nil), account.ProfileGrants...)
	if !containsString(grants, profile.Name) {
		grants = append(grants, profile.Name)
	}
	if _, err := controlPlaneA.UpdateServiceAccount(
		t.Context(),
		admin,
		account.ProjectID,
		account.ID,
		contracts.UpdateServiceAccountRequest{ProfileGrants: &grants},
	); err != nil {
		t.Fatal(err)
	}

	serverA := newUpgradeCompatibilityHTTPServer(t, controlPlaneA)
	initialClientA, err := initialv1client.NewClient(
		serverA.URL,
		credential,
		serverA.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	createOperation, err := initialClientA.CreateSandbox(
		t.Context(),
		"upgrade-create-initial",
		profile.Name,
		map[string]string{"client": "initial-v1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if createOperation.Kind != "create" || createOperation.SandboxID == "" {
		t.Fatalf("initial-v1 create operation = %#v", createOperation)
	}
	beforeReplacement, err := initialClientA.GetSandbox(
		t.Context(),
		createOperation.SandboxID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if beforeReplacement.ProfileRevisionID != profile.CurrentRevision.ID {
		t.Fatalf(
			"initial-v1 Sandbox ProfileRevision = %q, want %q",
			beforeReplacement.ProfileRevisionID,
			profile.CurrentRevision.ID,
		)
	}

	storeB, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storeB.Close)
	controlPlaneB := newControlPlaneService(t, storeB, generousQuota())
	revisedSpec := profileSpec
	revisedSpec.Resources.CPUMillis = 2000
	revisedProfile, err := controlPlaneB.ReviseProfile(
		t.Context(),
		admin,
		profile.Name,
		contracts.ReviseProfileRequest{Spec: revisedSpec},
	)
	if err != nil {
		t.Fatal(err)
	}
	serverB := newUpgradeCompatibilityHTTPServer(t, controlPlaneB)
	initialClientB, err := initialv1client.NewClient(
		serverB.URL,
		credential,
		serverB.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	afterReplacement, err := initialClientB.GetSandbox(
		t.Context(),
		createOperation.SandboxID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if afterReplacement.ProfileRevisionID != profile.CurrentRevision.ID {
		t.Fatalf(
			"rolling replacement rewrote pinned ProfileRevision to %q",
			afterReplacement.ProfileRevisionID,
		)
	}
	futureOperation, err := initialClientB.CreateSandbox(
		t.Context(),
		"upgrade-create-revised",
		profile.Name,
		map[string]string{"client": "initial-v1-after-replacement"},
	)
	if err != nil {
		t.Fatal(err)
	}
	futureSandbox, err := initialClientB.GetSandbox(t.Context(), futureOperation.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if futureSandbox.ProfileRevisionID != revisedProfile.CurrentRevision.ID ||
		futureSandbox.ProfileRevisionID == afterReplacement.ProfileRevisionID {
		t.Fatalf(
			"future/pinned ProfileRevisions = %q/%q, want %q and distinct",
			futureSandbox.ProfileRevisionID,
			afterReplacement.ProfileRevisionID,
			revisedProfile.CurrentRevision.ID,
		)
	}
	principalA := authenticateCredential(t, controlPlaneA, credential)
	if oldReplicaView, err := controlPlaneA.GetSandbox(
		t.Context(),
		principalA,
		futureSandbox.ID,
	); err != nil || oldReplicaView.ProfileRevisionID != revisedProfile.CurrentRevision.ID {
		t.Fatalf("coexisting control-plane replica view = %#v, error %v", oldReplicaView, err)
	}

	checkpointBytes := []byte("initial-v1-checkpoint-portable-bytes")
	checkpointSum := sha256.Sum256(checkpointBytes)
	checkpointSHA256 := hex.EncodeToString(checkpointSum[:])
	checkpointCompatibility["profileRevisionId"] = afterReplacement.ProfileRevisionID
	checkpoint := contracts.WorkspaceCheckpoint{
		ID:               "chk_upgrade_initial_v1",
		WorkspaceID:      afterReplacement.Workspace.ID,
		SourceGeneration: afterReplacement.Generation,
		SHA256:           checkpointSHA256, SizeBytes: int64(len(checkpointBytes)),
		Compatibility: checkpointCompatibility,
		RetainUntil:   upgradeCompatibilityNow().Add(24 * time.Hour),
		CreatedAt:     upgradeCompatibilityNow(),
	}
	publication := ports.CheckpointPublicationInput{
		Checkpoint:                  checkpoint,
		StorageKey:                  "checkpoints/initial-v1-upgrade",
		ExpectedWorkspaceGeneration: afterReplacement.Generation,
	}
	if _, err := storeA.StageCheckpoint(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.VerifyCheckpoint(t.Context(), publication, upgradeCompatibilityNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.PublishCheckpoint(t.Context(), publication, upgradeCompatibilityNow()); err != nil {
		t.Fatal(err)
	}
	checkpointStore := &checkpointObjectStore{objects: map[string][]byte{
		publication.StorageKey: bytes.Clone(checkpointBytes),
	}}
	restoreSender, err := lifecycle.NewCheckpointRestoreSender(
		t.Context(),
		lifecycle.CheckpointRestoreSenderConfig{
			DatabaseURL: integrationDatabaseURL,
			ObjectStore: checkpointStore,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restoreSender.Close)
	assignment := &runnerv1.AssignmentCommand{
		Fence: &runnerv1.AssignmentFence{
			AssignmentId:      "asn_upgrade_initial_v1",
			SandboxId:         beforeReplacement.ID,
			InstanceId:        "ins_upgrade_initial_v1",
			SandboxGeneration: uint64(beforeReplacement.Generation),
			FencingToken:      []byte("upgrade-fencing-token"),
		},
		ProfileRevisionId:  beforeReplacement.ProfileRevisionID,
		Requirements:       &runnerv1.ProfileRequirements{Architecture: "amd64"},
		SourceCheckpointId: checkpoint.ID,
		DeadlineUnixMs:     uint64(upgradeCompatibilityNow().Add(time.Minute).UnixMilli()),
		Correlation: &runnerv1.Correlation{
			RequestId:         "request-upgrade-restore",
			OperationId:       "operation-upgrade-restore",
			SandboxId:         beforeReplacement.ID,
			InstanceId:        "ins_upgrade_initial_v1",
			SandboxGeneration: uint64(beforeReplacement.Generation),
			AssignmentId:      "asn_upgrade_initial_v1",
			RunnerId:          "runner-after-upgrade",
		},
	}
	var restored bytes.Buffer
	var restoreBegin *runnerv1.RestoreBegin
	if err := restoreSender.StreamRestore(
		t.Context(),
		assignment,
		func(frame *runnerv1.ControlPlaneToRunner) error {
			if begin := frame.GetRestoreBegin(); begin != nil {
				restoreBegin = begin
			}
			if chunk := frame.GetRestoreChunk(); chunk != nil && len(chunk.Data) != 0 {
				_, err := restored.Write(chunk.Data)
				return err
			}
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if restoreBegin == nil ||
		restoreBegin.CheckpointId != checkpoint.ID ||
		restoreBegin.Compatibility["guestProtocolGeneration"] != "1" ||
		!bytes.Equal(restored.Bytes(), checkpointBytes) {
		t.Fatalf(
			"initial-v1 checkpoint restore begin=%#v bytes=%q",
			restoreBegin,
			restored.Bytes(),
		)
	}

	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var pinnedCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM secondbox.sandboxes
		WHERE id IN ($1,$2)
		  AND profile_revision_id IN ($3,$4)`,
		beforeReplacement.ID,
		futureSandbox.ID,
		profile.CurrentRevision.ID,
		revisedProfile.CurrentRevision.ID,
	).Scan(&pinnedCount); err != nil {
		t.Fatal(err)
	}
	if pinnedCount != 2 {
		t.Fatalf("durable ProfileRevision pins after replacement = %d, want 2", pinnedCount)
	}
}

func newUpgradeCompatibilityHTTPServer(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
) *httptest.Server {
	t.Helper()
	handler, err := api.NewHandler(api.HandlerConfig{
		Service:                   controlPlane,
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func readInitialV1Fixture[T any](t *testing.T, name string) T {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate upgrade compatibility fixture")
	}
	content, err := os.ReadFile(filepath.Join(
		filepath.Dir(sourceFile),
		"..",
		"compatibility",
		name,
	))
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err := json.Unmarshal(content, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func newUpgradeCompatibilityDatabase(t *testing.T, suffix string) string {
	t.Helper()
	targetConfig, err := pgx.ParseConfig(integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig := targetConfig.Copy()
	adminConfig.Database = "postgres"
	adminConnection, err := pgx.ConnectConfig(t.Context(), adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := adminConnection.Close(context.Background()); err != nil {
			t.Errorf("close upgrade database administrator connection: %v", err)
		}
	})
	databaseName := fmt.Sprintf(
		"secondbox_upgrade_%s_%d",
		suffix,
		integrationIdentitySequence.Add(1),
	)
	if _, err := adminConnection.Exec(
		t.Context(),
		"CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()+" TEMPLATE template0",
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := adminConnection.Exec(
			context.Background(),
			"DROP DATABASE "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)",
		); err != nil {
			t.Errorf("drop upgrade compatibility database %s: %v", databaseName, err)
		}
	})
	targetConfig.Database = databaseName
	parsedURL, err := url.Parse(integrationDatabaseURL)
	if err != nil || parsedURL.Scheme == "" {
		t.Fatalf("upgrade compatibility database URL is not a PostgreSQL URL: %v", err)
	}
	parsedURL.Path = "/" + databaseName
	parsedURL.RawPath = ""
	return parsedURL.String()
}

func assertUpgradeCompatibilityMigrationLedger(t *testing.T, databaseURL string) {
	t.Helper()
	connection, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(t.Context())
	var version, checksum string
	if err := connection.QueryRow(t.Context(), `
		SELECT version,checksum_sha256
		FROM secondbox.schema_migrations`,
	).Scan(&version, &checksum); err != nil {
		t.Fatal(err)
	}
	if version != "0001_secondbox" || checksum != initialV1MigrationSHA256(t) {
		t.Fatalf("migration ledger = %q %q", version, checksum)
	}
	var tableCount int
	if err := connection.QueryRow(t.Context(), `
		SELECT count(*)
		FROM pg_tables
		WHERE schemaname='secondbox'`,
	).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount < 30 {
		t.Fatalf("fresh migrated SecondBox table count = %d, want at least 30", tableCount)
	}
}

func applyInitialV1RawMigration(t *testing.T, databaseURL string) {
	t.Helper()
	connection, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	migrationSQL, err := os.ReadFile(initialV1MigrationPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(t.Context(), string(migrationSQL)); err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func initialV1MigrationPath(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate initial v1 migration")
	}
	return filepath.Join(
		filepath.Dir(sourceFile),
		"..",
		"..",
		"migrations",
		"postgres",
		"0001_secondbox.sql",
	)
}

func initialV1MigrationSHA256(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(initialV1MigrationPath(t))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func upgradeCompatibilityNow() time.Time {
	return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
}
