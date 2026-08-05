package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

var requiredControlPlaneEnvironment = map[string]string{
	"SECONDBOX_LISTEN_ADDR":                               "127.0.0.1:8080",
	"SECONDBOX_PUBLIC_BASE_URL":                           "http://127.0.0.1:8080",
	"SECONDBOX_RUNNER_LISTEN_ADDR":                        "127.0.0.1:9443",
	"SECONDBOX_DATABASE_URL":                              "postgres://secondbox@example/secondbox",
	"SECONDBOX_LOG_PATH":                                  "/tmp/secondbox.log",
	"SECONDBOX_PLATFORM_TOKEN":                            "platform-token-0000000000000000",
	"SECONDBOX_RUNNER_CREDENTIAL":                         "runner-credential-00000000000000000000",
	"SECONDBOX_APPLICATION_AUTHORITIES_JSON":              "[]",
	"SECONDBOX_RUNNER_SERVER_CERTIFICATE":                 "/tmp/server.crt",
	"SECONDBOX_RUNNER_SERVER_PRIVATE_KEY":                 "/tmp/server.key",
	"SECONDBOX_RUNNER_CA_CERTIFICATE":                     "/tmp/ca.crt",
	"SECONDBOX_SIGNED_ASSET_CATALOG_PATH":                 "/tmp/assets.json",
	"SECONDBOX_OBJECT_STORE_ENDPOINT":                     "http://object-store:9000",
	"SECONDBOX_OBJECT_STORE_REGION":                       "us-east-1",
	"SECONDBOX_OBJECT_STORE_BUCKET":                       "secondbox",
	"SECONDBOX_OBJECT_STORE_ROOT_USER":                    "secondbox",
	"SECONDBOX_OBJECT_STORE_ROOT_PASSWORD":                "object-password-000000000000",
	"SECONDBOX_OBJECT_STORE_USE_PATH_STYLE":               "true",
	"SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY":               "/tmp",
	"SECONDBOX_DATA_PLANE_RETENTION_SECONDS":              "86400",
	"SECONDBOX_DEFAULT_SUBJECT_MAX_SANDBOXES":             "100",
	"SECONDBOX_DEFAULT_SUBJECT_MAX_ACTIVE_INSTANCES":      "20",
	"SECONDBOX_DEFAULT_SUBJECT_MAX_CPU_MILLIS":            "80000",
	"SECONDBOX_DEFAULT_SUBJECT_MAX_MEMORY_BYTES":          "171798691840",
	"SECONDBOX_DEFAULT_SUBJECT_MAX_ARTIFACT_BYTES":        "1099511627776",
	"SECONDBOX_DEFAULT_SUBJECT_MAX_SNAPSHOTS":             "500",
	"SECONDBOX_DEFAULT_SUBJECT_MAX_ARTIFACTS":             "5000",
	"SECONDBOX_DEFAULT_SUBJECT_MAX_PORT_SESSIONS":         "100",
	"SECONDBOX_DEFAULT_SUBJECT_MAX_CONCURRENT_OPERATIONS": "20",
	"SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS":     "250",
	"SECONDBOX_RUNNER_COMMAND_POLL_INTERVAL_MILLISECONDS": "250",
	"SECONDBOX_RUNNER_ENABLED_FEATURES":                   "exec-streaming,file-streaming,pty,evidence,local-workspace,port-proxy",
}

func setRequiredControlPlaneEnvironment(t *testing.T) {
	t.Helper()
	for name, value := range requiredControlPlaneEnvironment {
		t.Setenv(name, value)
	}
	for _, name := range CategoryCEnvironmentNames() {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Unsetenv("SECONDBOX_RUNNER_PROTOCOL_MINIMUM")
	_ = os.Unsetenv("SECONDBOX_RUNNER_PROTOCOL_MAXIMUM")
}

func TestFromEnvironmentRequiresExactlyDeploymentAuthorityAndContestedSettings(t *testing.T) {
	if got := len(requiredControlPlaneEnvironment); got != 32 {
		t.Fatalf("required environment count = %d, want 32", got)
	}
	for absent := range requiredControlPlaneEnvironment {
		t.Run(absent, func(t *testing.T) {
			setRequiredControlPlaneEnvironment(t)
			if err := os.Unsetenv(absent); err != nil {
				t.Fatal(err)
			}
			_, err := FromEnvironment()
			if err == nil || !strings.Contains(err.Error(), absent) {
				t.Fatalf("error = %v, want missing %s", err, absent)
			}
		})
	}
}

func TestEnvironmentSurfaceHasAnExplicitFinalCategory(t *testing.T) {
	categoryB := []string{
		"SECONDBOX_RUNNER_PROTOCOL_MINIMUM", "SECONDBOX_RUNNER_PROTOCOL_MAXIMUM",
	}
	seen := make(map[string]string)
	for name := range requiredControlPlaneEnvironment {
		seen[name] = "required authority or contested policy"
	}
	for _, name := range CategoryCEnvironmentNames() {
		if previous := seen[name]; previous != "" {
			t.Fatalf("%s appears in both %s and tuning", name, previous)
		}
		seen[name] = "optional tuning override"
	}
	for _, name := range categoryB {
		if previous := seen[name]; previous != "" {
			t.Fatalf("%s appears in both %s and removed facts", name, previous)
		}
		seen[name] = "removed compiled fact"
	}
	if got := len(seen); got != 53 {
		t.Fatalf("classified environment surface = %d names, want 53", got)
	}
}

func TestFromEnvironmentUsesLiteralTuningDefaultsAndValidatedOverrides(t *testing.T) {
	setRequiredControlPlaneEnvironment(t)
	got, err := FromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if got.HTTPTimeout != 30*time.Second || got.RunnerHeartbeatInterval != 5000*time.Millisecond ||
		got.RunnerHeartbeatTimeout != 30000*time.Millisecond || got.RunnerCommandDeliveryBatchSize != 16 ||
		got.RunnerEventPersistenceBatchSize != 16 || got.RunnerEventPersistenceBatchWait != 2*time.Millisecond ||
		got.DataPlaneMaximumSessionBytes != 67108864 || got.IdempotencyRetention != 24*time.Hour ||
		got.LifecycleReconcileBatchSize != 8 ||
		got.LifecycleReconcilePollInterval != 250*time.Millisecond ||
		got.LifecycleReconcileClaimDuration != 30000*time.Millisecond ||
		got.GarbageCollectionPollInterval != 60000*time.Millisecond ||
		got.AssignmentClaimDuration != 30000*time.Millisecond || got.AssignmentDeadline != 120000*time.Millisecond ||
		got.AssignmentRetryLimit != 2 || got.SchedulerSerializationRetryLimit != 3 ||
		got.ObjectStoreRetryMaxAttempts != 3 || got.ObjectStoreHTTPTimeout != 30000*time.Millisecond ||
		got.ObjectStoreMaxObjectBytes != 10737418240 {
		t.Fatalf("unexpected tuning defaults: %+v", got)
	}

	t.Setenv("SECONDBOX_HTTP_TIMEOUT_SECONDS", "41")
	got, err = FromEnvironment()
	if err != nil || got.HTTPTimeout != 41*time.Second {
		t.Fatalf("override result = %v, %v", got.HTTPTimeout, err)
	}
	t.Setenv("SECONDBOX_HTTP_TIMEOUT_SECONDS", "invalid")
	if _, err := FromEnvironment(); err == nil || !strings.Contains(err.Error(), "SECONDBOX_HTTP_TIMEOUT_SECONDS") {
		t.Fatalf("invalid override error = %v", err)
	}
}

// An unusable override must fail closed and name itself. Falling back to the
// compiled constant would silently run a deployment on a value it did not
// choose, which is the failure mode the tuning defaults exist to avoid.
func TestFromEnvironmentRejectsEveryUnusableTuningOverride(t *testing.T) {
	if len(TuningDefaults()) != 19 {
		t.Fatalf("tuning surface = %d, want 19", len(TuningDefaults()))
	}
	for _, tuning := range TuningDefaults() {
		for _, unusable := range []string{"invalid", "-1", ""} {
			t.Run(tuning.Environment+"/"+unusable, func(t *testing.T) {
				setRequiredControlPlaneEnvironment(t)
				t.Setenv(tuning.Environment, unusable)
				if _, err := FromEnvironment(); err != nil {
					if !strings.Contains(err.Error(), tuning.Environment) {
						t.Fatalf("error = %v, want it to name %s", err, tuning.Environment)
					}
					return
				}
				t.Fatalf("accepted %q for %s instead of failing closed", unusable, tuning.Environment)
			})
		}
	}
}

func TestFromEnvironmentDoesNotReadProtocolVersionDeclarations(t *testing.T) {
	setRequiredControlPlaneEnvironment(t)
	t.Setenv("SECONDBOX_RUNNER_PROTOCOL_MINIMUM", "999")
	t.Setenv("SECONDBOX_RUNNER_PROTOCOL_MAXIMUM", "0")
	if _, err := FromEnvironment(); err != nil {
		t.Fatal(err)
	}
}
