// Package config loads required standalone SecondBox process configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// Code-owned tuning defaults. Each corresponding environment variable is an
// optional, validated override; these literals are the reviewed deployed values.
const (
	DefaultHTTPTimeoutSeconds                          int64 = 30
	DefaultRunnerHeartbeatIntervalMilliseconds         int64 = 5000
	DefaultRunnerHeartbeatTimeoutMilliseconds          int64 = 30000
	DefaultRunnerCommandDeliveryBatchSize              int64 = 16
	DefaultRunnerEventPersistenceBatchSize             int64 = 16
	DefaultRunnerEventPersistenceBatchWaitMilliseconds int64 = 2
	DefaultDataPlaneMaximumSessionBytes                int64 = 67108864
	DefaultIdempotencyRetentionSeconds                 int64 = 86400
	DefaultLifecycleReconcileBatchSize                 int64 = 8
	DefaultLifecycleReconcilePollIntervalMilliseconds  int64 = 250
	DefaultLifecycleReconcileClaimDurationMilliseconds int64 = 30000
	DefaultAssignmentClaimDurationMilliseconds         int64 = 30000
	DefaultAssignmentDeadlineMilliseconds              int64 = 120000
	DefaultAssignmentRetryLimit                        int64 = 2
	DefaultSchedulerSerializationRetryLimit            int64 = 3
)

// Config contains only explicitly configured control-plane settings.
type Config struct {
	ListenAddress                    string
	PublicBaseURL                    string
	RunnerListenAddress              string
	DatabaseURL                      string
	LogPath                          string
	PlatformToken                    string
	HTTPTimeout                      time.Duration
	RunnerServerCertificatePath      string
	RunnerServerPrivateKeyPath       string
	RunnerCACertificatePath          string
	RunnerCredential                 string
	RunnerHeartbeatInterval          time.Duration
	RunnerCommandPollInterval        time.Duration
	RunnerCommandDeliveryBatchSize   int64
	RunnerEventPersistenceBatchSize  int
	RunnerEventPersistenceBatchWait  time.Duration
	DataPlanePollInterval            time.Duration
	DataPlaneRetention               time.Duration
	DataPlaneMaximumSessionBytes     int64
	IdempotencyRetention             time.Duration
	LifecycleReconcileBatchSize      int
	LifecycleReconcilePollInterval   time.Duration
	LifecycleReconcileClaimDuration  time.Duration
	AssignmentClaimDuration          time.Duration
	AssignmentDeadline               time.Duration
	RunnerHeartbeatTimeout           time.Duration
	AssignmentRetryLimit             int64
	SchedulerSerializationRetryLimit int
	AssetCatalogPath                 string
	RunnerEnabledFeatures            []string
	DefaultSubjectQuota              contracts.QuotaLimits
}

// FromEnvironment fails when any required setting is absent or invalid.
func FromEnvironment() (Config, error) {
	listenAddress, err := requiredString("SECONDBOX_LISTEN_ADDR")
	if err != nil {
		return Config{}, err
	}
	publicBaseURL, err := requiredHTTPURL("SECONDBOX_PUBLIC_BASE_URL")
	if err != nil {
		return Config{}, err
	}
	runnerListenAddress, err := requiredString("SECONDBOX_RUNNER_LISTEN_ADDR")
	if err != nil {
		return Config{}, err
	}
	databaseURL, err := requiredString("SECONDBOX_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	logPath, err := requiredString("SECONDBOX_LOG_PATH")
	if err != nil {
		return Config{}, err
	}
	if !filepath.IsAbs(logPath) {
		return Config{}, errorsForEnvironment("SECONDBOX_LOG_PATH must be an absolute path")
	}
	platformToken, err := requiredSecret("SECONDBOX_PLATFORM_TOKEN", 24)
	if err != nil {
		return Config{}, err
	}
	runnerCredential, err := requiredSecret("SECONDBOX_RUNNER_CREDENTIAL", 32)
	if err != nil {
		return Config{}, err
	}
	runnerServerCertificatePath, err := requiredAbsolutePath("SECONDBOX_RUNNER_SERVER_CERTIFICATE")
	if err != nil {
		return Config{}, err
	}
	runnerServerPrivateKeyPath, err := requiredAbsolutePath("SECONDBOX_RUNNER_SERVER_PRIVATE_KEY")
	if err != nil {
		return Config{}, err
	}
	runnerCACertificatePath, err := requiredAbsolutePath("SECONDBOX_RUNNER_CA_CERTIFICATE")
	if err != nil {
		return Config{}, err
	}
	httpSeconds, err := optionalPositiveInt64("SECONDBOX_HTTP_TIMEOUT_SECONDS", DefaultHTTPTimeoutSeconds)
	if err != nil {
		return Config{}, err
	}
	runnerHeartbeatMilliseconds, err := optionalPositiveInt64("SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS", DefaultRunnerHeartbeatIntervalMilliseconds)
	if err != nil {
		return Config{}, err
	}
	runnerCommandPollMilliseconds, err := requiredPositiveInt64("SECONDBOX_RUNNER_COMMAND_POLL_INTERVAL_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	runnerCommandDeliveryBatchSize, err := optionalPositiveInt64("SECONDBOX_RUNNER_COMMAND_DELIVERY_BATCH_SIZE", DefaultRunnerCommandDeliveryBatchSize)
	if err != nil {
		return Config{}, err
	}
	runnerEventPersistenceBatchSize, err := optionalPositiveInt64("SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_SIZE", DefaultRunnerEventPersistenceBatchSize)
	if err != nil {
		return Config{}, err
	}
	runnerEventPersistenceBatchSizeInt := int(runnerEventPersistenceBatchSize)
	if int64(runnerEventPersistenceBatchSizeInt) != runnerEventPersistenceBatchSize {
		return Config{}, errorsForEnvironment("runner event persistence batch size exceeds process integer range")
	}
	runnerEventPersistenceBatchWaitMilliseconds, err := optionalPositiveInt64("SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_WAIT_MILLISECONDS", DefaultRunnerEventPersistenceBatchWaitMilliseconds)
	if err != nil {
		return Config{}, err
	}
	dataPlanePollMilliseconds, err := requiredPositiveInt64("SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	dataPlaneRetentionSeconds, err := requiredPositiveInt64("SECONDBOX_DATA_PLANE_RETENTION_SECONDS")
	if err != nil {
		return Config{}, err
	}
	dataPlaneMaximumSessionBytes, err := optionalPositiveInt64("SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES", DefaultDataPlaneMaximumSessionBytes)
	if err != nil {
		return Config{}, err
	}
	idempotencyRetentionSeconds, err := optionalPositiveInt64("SECONDBOX_IDEMPOTENCY_RETENTION_SECONDS", DefaultIdempotencyRetentionSeconds)
	if err != nil {
		return Config{}, err
	}
	lifecycleReconcileBatchSize, err := optionalPositiveInt64("SECONDBOX_LIFECYCLE_RECONCILE_BATCH_SIZE", DefaultLifecycleReconcileBatchSize)
	if err != nil {
		return Config{}, err
	}
	lifecycleReconcileBatchSizeInt := int(lifecycleReconcileBatchSize)
	if int64(lifecycleReconcileBatchSizeInt) != lifecycleReconcileBatchSize {
		return Config{}, errorsForEnvironment("lifecycle reconcile batch size exceeds process integer range")
	}
	lifecycleReconcilePollMilliseconds, err := optionalPositiveInt64("SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS", DefaultLifecycleReconcilePollIntervalMilliseconds)
	if err != nil {
		return Config{}, err
	}
	lifecycleReconcileClaimMilliseconds, err := optionalPositiveInt64("SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS", DefaultLifecycleReconcileClaimDurationMilliseconds)
	if err != nil {
		return Config{}, err
	}
	assignmentClaimMilliseconds, err := optionalPositiveInt64("SECONDBOX_ASSIGNMENT_CLAIM_DURATION_MILLISECONDS", DefaultAssignmentClaimDurationMilliseconds)
	if err != nil {
		return Config{}, err
	}
	assignmentDeadlineMilliseconds, err := optionalPositiveInt64("SECONDBOX_ASSIGNMENT_DEADLINE_MILLISECONDS", DefaultAssignmentDeadlineMilliseconds)
	if err != nil {
		return Config{}, err
	}
	runnerHeartbeatTimeoutMilliseconds, err := optionalPositiveInt64("SECONDBOX_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS", DefaultRunnerHeartbeatTimeoutMilliseconds)
	if err != nil {
		return Config{}, err
	}
	assignmentRetryLimit, err := optionalNonNegativeInt64("SECONDBOX_ASSIGNMENT_RETRY_LIMIT", DefaultAssignmentRetryLimit)
	if err != nil {
		return Config{}, err
	}
	schedulerSerializationRetryLimit, err := optionalNonNegativeInt64("SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT", DefaultSchedulerSerializationRetryLimit)
	if err != nil {
		return Config{}, err
	}
	schedulerSerializationRetryLimitInt := int(schedulerSerializationRetryLimit)
	if int64(schedulerSerializationRetryLimitInt) != schedulerSerializationRetryLimit {
		return Config{}, errorsForEnvironment("scheduler serialization retry limit exceeds process integer range")
	}
	signedAssetCatalogPath, err := requiredAbsolutePath("SECONDBOX_SIGNED_ASSET_CATALOG_PATH")
	if err != nil {
		return Config{}, err
	}
	runnerEnabledFeatures, err := requiredCSV("SECONDBOX_RUNNER_ENABLED_FEATURES")
	if err != nil {
		return Config{}, err
	}
	subjectQuota, err := requiredQuota("SECONDBOX_DEFAULT_SUBJECT")
	if err != nil {
		return Config{}, err
	}
	return Config{
		ListenAddress: listenAddress, PublicBaseURL: publicBaseURL, RunnerListenAddress: runnerListenAddress,
		DatabaseURL: databaseURL, LogPath: logPath,
		PlatformToken:                    platformToken,
		HTTPTimeout:                      time.Duration(httpSeconds) * time.Second,
		RunnerServerCertificatePath:      runnerServerCertificatePath,
		RunnerServerPrivateKeyPath:       runnerServerPrivateKeyPath,
		RunnerCACertificatePath:          runnerCACertificatePath,
		RunnerCredential:                 runnerCredential,
		RunnerHeartbeatInterval:          time.Duration(runnerHeartbeatMilliseconds) * time.Millisecond,
		RunnerCommandPollInterval:        time.Duration(runnerCommandPollMilliseconds) * time.Millisecond,
		RunnerCommandDeliveryBatchSize:   runnerCommandDeliveryBatchSize,
		RunnerEventPersistenceBatchSize:  runnerEventPersistenceBatchSizeInt,
		RunnerEventPersistenceBatchWait:  time.Duration(runnerEventPersistenceBatchWaitMilliseconds) * time.Millisecond,
		DataPlanePollInterval:            time.Duration(dataPlanePollMilliseconds) * time.Millisecond,
		DataPlaneRetention:               time.Duration(dataPlaneRetentionSeconds) * time.Second,
		DataPlaneMaximumSessionBytes:     dataPlaneMaximumSessionBytes,
		IdempotencyRetention:             time.Duration(idempotencyRetentionSeconds) * time.Second,
		LifecycleReconcileBatchSize:      lifecycleReconcileBatchSizeInt,
		LifecycleReconcilePollInterval:   time.Duration(lifecycleReconcilePollMilliseconds) * time.Millisecond,
		LifecycleReconcileClaimDuration:  time.Duration(lifecycleReconcileClaimMilliseconds) * time.Millisecond,
		AssignmentClaimDuration:          time.Duration(assignmentClaimMilliseconds) * time.Millisecond,
		AssignmentDeadline:               time.Duration(assignmentDeadlineMilliseconds) * time.Millisecond,
		RunnerHeartbeatTimeout:           time.Duration(runnerHeartbeatTimeoutMilliseconds) * time.Millisecond,
		AssignmentRetryLimit:             assignmentRetryLimit,
		SchedulerSerializationRetryLimit: schedulerSerializationRetryLimitInt,
		AssetCatalogPath:                 signedAssetCatalogPath,
		RunnerEnabledFeatures:            runnerEnabledFeatures,
		DefaultSubjectQuota:              subjectQuota,
	}, nil
}

func requiredHTTPURL(name string) (string, error) {
	raw, err := requiredString(name)
	if err != nil {
		return "", err
	}
	value, err := url.Parse(raw)
	if err != nil || value.Host == "" || (value.Scheme != "http" && value.Scheme != "https") ||
		value.User != nil || value.RawQuery != "" || value.Fragment != "" {
		return "", errorsForEnvironment(name + " must be an absolute HTTP or HTTPS URL")
	}
	return value.String(), nil
}

func requiredAbsolutePath(name string) (string, error) {
	path, err := requiredString(name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		return "", errorsForEnvironment(name + " must be an absolute path")
	}
	return path, nil
}

func requiredCSV(name string) ([]string, error) {
	raw, err := requiredString(name)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" || seen[value] {
			return nil, fmt.Errorf("SecondBox environment variable %s must contain unique non-empty comma-separated values", name)
		}
		seen[value] = true
		values = append(values, value)
	}
	return values, nil
}

func requiredQuota(prefix string) (contracts.QuotaLimits, error) {
	names := []string{
		"MAX_SANDBOXES", "MAX_ACTIVE_INSTANCES", "MAX_VCPU_COUNT", "MAX_MEMORY_BYTES",
		"MAX_SNAPSHOTS", "MAX_PORT_SESSIONS",
		"MAX_CONCURRENT_OPERATIONS",
	}
	values := make([]int64, len(names))
	for index, suffix := range names {
		value, err := requiredNonNegativeInt64(prefix + "_" + suffix)
		if err != nil {
			return contracts.QuotaLimits{}, err
		}
		values[index] = value
	}
	return contracts.QuotaLimits{
		MaxSandboxes: values[0], MaxActiveInstances: values[1], MaxVCPUCount: values[2],
		MaxMemoryBytes: values[3], MaxSnapshots: values[4], MaxPortSessions: values[5],
		MaxConcurrentOperations: values[6],
	}, nil
}

func requiredString(name string) (string, error) {
	value, exists := os.LookupEnv(name)
	if !exists || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("SecondBox missing required environment variable %s", name)
	}
	return value, nil
}

func requiredSecret(name string, minimumLength int) (string, error) {
	value, err := requiredString(name)
	if err != nil {
		return "", err
	}
	if len(value) < minimumLength {
		return "", fmt.Errorf("SecondBox environment variable %s must contain at least %d bytes", name, minimumLength)
	}
	return value, nil
}

func requiredNonNegativeInt64(name string) (int64, error) {
	raw, err := requiredString(name)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("SecondBox environment variable %s must be a non-negative integer", name)
	}
	return value, nil
}

func requiredPositiveInt64(name string) (int64, error) {
	raw, err := requiredString(name)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("SecondBox environment variable %s must be a positive integer", name)
	}
	return value, nil
}

func optionalPositiveInt64(name string, fallback int64) (int64, error) {
	if _, exists := os.LookupEnv(name); !exists {
		return fallback, nil
	}
	return requiredPositiveInt64(name)
}

func optionalNonNegativeInt64(name string, fallback int64) (int64, error) {
	if _, exists := os.LookupEnv(name); !exists {
		return fallback, nil
	}
	return requiredNonNegativeInt64(name)
}

func errorsForEnvironment(message string) error {
	return fmt.Errorf("SecondBox environment configuration invalid: %s", message)
}
