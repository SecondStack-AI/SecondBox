package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

var qualificationContainerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]+$`)

func TestQualifiedPostgresRestartRequeuesEveryLocalWorkspaceOperation(t *testing.T) {
	if os.Getenv("SECONDBOX_QUALIFY_POSTGRES_RESTART") != "1" {
		t.Skip("set SECONDBOX_QUALIFY_POSTGRES_RESTART=1 for destructive disposable PostgreSQL restart qualification")
	}
	containerName := strings.TrimSpace(
		os.Getenv("SECONDBOX_QUALIFY_POSTGRES_RESTART_CONTAINER"),
	)
	if !qualificationContainerName.MatchString(containerName) {
		t.Fatal("SECONDBOX_QUALIFY_POSTGRES_RESTART_CONTAINER must be one exact Docker container name")
	}
	rawURL := strings.TrimSpace(os.Getenv("SECONDBOX_TEST_DATABASE_URL"))
	if rawURL == "" {
		t.Fatal("SECONDBOX_TEST_DATABASE_URL is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := fmt.Sprintf(
		"secondbox_postgres_restart_qualification_%d",
		os.Getpid(),
	)
	adminURL := *parsed
	adminURL.Path = "/postgres"
	currentAdminURL := adminURL.String()
	admin, err := pgx.Connect(t.Context(), adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		admin.Close(t.Context())
		t.Fatal(err)
	}
	if err := admin.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	testURL := *parsed
	testURL.Path = "/" + databaseName
	currentTestURL := testURL.String()
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupAdmin, cleanupErr := connectPostgresQualification(
			cleanupContext,
			currentAdminURL,
			30*time.Second,
		)
		if cleanupErr != nil {
			t.Errorf("reconnect PostgreSQL for qualification cleanup: %v", cleanupErr)
			return
		}
		defer cleanupAdmin.Close(cleanupContext)
		if _, cleanupErr := cleanupAdmin.Exec(
			cleanupContext,
			"DROP DATABASE "+identifier+" WITH (FORCE)",
		); cleanupErr != nil {
			t.Errorf("drop PostgreSQL restart qualification database: %v", cleanupErr)
		}
	})
	if err := postgresmigrations.Apply(t.Context(), currentTestURL); err != nil {
		t.Fatal(err)
	}
	store, err := NewPostgresStateStore(t.Context(), currentTestURL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)
	for index, kind := range allPostgresLocalWorkspaceCommandKinds() {
		command := &runnerv1.LocalWorkspaceCommand{
			MessageId: "qualified-restart-" + kind.String(),
			Sequence:  uint64(index + 1), CommandVersion: 1, Kind: kind,
			OperationId: "operation-" + kind.String(),
			EffectId:    "effect-" + kind.String(),
			SandboxId:   "sandbox-home", WorkspaceId: "workspace-home",
			SnapshotId: "snapshot-home", ExpectedGeneration: 4, NextGeneration: 5,
			LogicalCapacityBytes: 16 << 20,
			FencingToken:         []byte("01234567890123456789012345678901"),
			Correlation: &runnerv1.Correlation{
				OperationId: "operation-" + kind.String(),
				SandboxId:   "sandbox-home",
				RunnerId:    "runner-home",
			},
		}
		payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{
				LocalWorkspace: command,
			},
		})
		if err != nil {
			store.Close()
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(t.Context(), `
			INSERT INTO secondbox.runner_commands (
				id,runner_id,assignment_id,kind,payload,state,target_connection_id,
				delivery_count,created_at,updated_at,delivered_at
			) VALUES ($1,'runner-home',$2,'local-workspace',$3,'delivered',
			          'connection-before-restart',1,$4,$4,$4)`,
			command.MessageId,
			command.EffectId,
			payload,
			now,
		); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	store.Close()

	restart := exec.Command("docker", "restart", containerName)
	if output, err := restart.CombinedOutput(); err != nil {
		t.Fatalf("restart disposable PostgreSQL container: %v: %s", err, output)
	}
	postRestartBaseURL, err := postgresQualificationContainerURL(
		rawURL,
		containerName,
	)
	if err != nil {
		t.Fatal(err)
	}
	postRestartAdminURL := *postRestartBaseURL
	postRestartAdminURL.Path = "/postgres"
	currentAdminURL = postRestartAdminURL.String()
	postRestartTestURL := *postRestartBaseURL
	postRestartTestURL.Path = "/" + databaseName
	currentTestURL = postRestartTestURL.String()
	restarted, err := connectRunnerStateQualification(
		t.Context(),
		currentTestURL,
		30*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.OpenConnection(
		t.Context(),
		RunnerIdentity{
			RunnerID:         "runner-home",
			CredentialSerial: "credential-after-postgres-restart",
		},
		"connection-after-restart",
		1,
		now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	rows, err := restarted.pool.Query(t.Context(), `
		SELECT id,payload,state,target_connection_id
		FROM secondbox.runner_commands
		WHERE runner_id='runner-home' AND kind='local-workspace'
		  AND id<>'workspace-reconcile-connection-after-restart'
		ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := make(map[runnerv1.LocalWorkspaceCommandKind]bool)
	for rows.Next() {
		var id, state, targetConnectionID string
		var payload []byte
		if err := rows.Scan(&id, &payload, &state, &targetConnectionID); err != nil {
			t.Fatal(err)
		}
		var envelope runnerv1.ControlPlaneToRunner
		if err := proto.Unmarshal(payload, &envelope); err != nil {
			t.Fatal(err)
		}
		command := envelope.GetLocalWorkspace()
		if command == nil ||
			id != "qualified-restart-"+command.Kind.String() ||
			state != "pending" ||
			targetConnectionID != "" {
			t.Fatalf(
				"post-restart command id=%q state=%q target=%q payload=%#v",
				id,
				state,
				targetConnectionID,
				command,
			)
		}
		seen[command.Kind] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range allPostgresLocalWorkspaceCommandKinds() {
		if !seen[kind] {
			t.Errorf("PostgreSQL restart lost %s", kind)
		}
	}
}

func postgresQualificationContainerURL(
	databaseURL string,
	containerName string,
) (*url.URL, error) {
	published := exec.Command("docker", "port", containerName, "5432/tcp")
	output, err := published.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("resolve restarted PostgreSQL published port: %w: %s", err, output)
	}
	lines := strings.Fields(string(output))
	if len(lines) == 0 {
		return nil, errors.New("restarted PostgreSQL container has no published 5432/tcp port")
	}
	_, port, err := net.SplitHostPort(lines[0])
	if err != nil {
		return nil, fmt.Errorf("parse restarted PostgreSQL published port %q: %w", lines[0], err)
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		return nil, err
	}
	host := parsed.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	parsed.Host = net.JoinHostPort(host, port)
	return parsed, nil
}

func connectRunnerStateQualification(
	ctx context.Context,
	databaseURL string,
	timeout time.Duration,
) (*PostgresStateStore, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		store, err := NewPostgresStateStore(ctx, databaseURL)
		if err == nil {
			return store, nil
		}
		lastErr = err
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("PostgreSQL did not return before qualification deadline: %w", lastErr)
}

func connectPostgresQualification(
	ctx context.Context,
	databaseURL string,
	timeout time.Duration,
) (*pgx.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		connection, err := pgx.Connect(ctx, databaseURL)
		if err == nil {
			return connection, nil
		}
		lastErr = err
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("PostgreSQL did not return before qualification deadline: %w", lastErr)
}
