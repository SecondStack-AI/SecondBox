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

// Config contains only explicitly configured control-plane settings.
type Config struct {
	ListenAddress                       string
	PublicBaseURL                       string
	RunnerListenAddress                 string
	DatabaseURL                         string
	LogPath                             string
	BootstrapAdminToken                 string
	APIKeyHashSecret                    string
	HTTPTimeout                         time.Duration
	RunnerServerCertificatePath         string
	RunnerServerPrivateKeyPath          string
	RunnerCACertificatePath             string
	RunnerCAPrivateKeyPath              string
	RunnerEnrollmentHashSecret          string
	RunnerCertificateLifetime           time.Duration
	RunnerCredentialVerificationTimeout time.Duration
	RunnerHeartbeatInterval             time.Duration
	RunnerCommandPollInterval           time.Duration
	DataPlanePollInterval               time.Duration
	DataPlaneClaimDuration              time.Duration
	DataPlaneRetention                  time.Duration
	DataPlaneMaximumFrameBytes          int64
	DataPlaneMaximumSessionBytes        int64
	LifecycleReconcilePollInterval      time.Duration
	LifecycleReconcileClaimDuration     time.Duration
	AssignmentClaimDuration             time.Duration
	AssignmentDeadline                  time.Duration
	RunnerHeartbeatTimeout              time.Duration
	AssignmentRetryLimit                int64
	SchedulerSerializationRetryLimit    int
	SignedAssetCatalogPath              string
	ObjectStoreEndpoint                 string
	ObjectStoreRegion                   string
	ObjectStoreBucket                   string
	ObjectStoreAccessKeyID              string
	ObjectStoreSecretAccessKey          string
	ObjectStoreUsePathStyle             bool
	ObjectStoreRetryMaxAttempts         int
	ObjectStoreHTTPTimeout              time.Duration
	ObjectStoreTempDirectory            string
	ObjectStoreMaxObjectBytes           int64
	CheckpointSpoolDirectory            string
	RunnerProtocolMinimum               uint32
	RunnerProtocolMaximum               uint32
	RunnerEnabledFeatures               []string
	DefaultProjectQuota                 contracts.QuotaLimits
	DefaultProfileQuota                 contracts.QuotaLimits
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
	bootstrapToken, err := requiredSecret("SECONDBOX_BOOTSTRAP_ADMIN_TOKEN", 24)
	if err != nil {
		return Config{}, err
	}
	hashSecret, err := requiredSecret("SECONDBOX_API_KEY_HASH_SECRET", 32)
	if err != nil {
		return Config{}, err
	}
	runnerEnrollmentHashSecret, err := requiredSecret("SECONDBOX_RUNNER_ENROLLMENT_HASH_SECRET", 32)
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
	runnerCAPrivateKeyPath, err := requiredAbsolutePath("SECONDBOX_RUNNER_CA_PRIVATE_KEY")
	if err != nil {
		return Config{}, err
	}
	httpSeconds, err := requiredPositiveInt64("SECONDBOX_HTTP_TIMEOUT_SECONDS")
	if err != nil {
		return Config{}, err
	}
	runnerCertificateLifetimeSeconds, err := requiredPositiveInt64("SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_SECONDS")
	if err != nil {
		return Config{}, err
	}
	runnerHeartbeatMilliseconds, err := requiredPositiveInt64("SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	runnerCommandPollMilliseconds, err := requiredPositiveInt64("SECONDBOX_RUNNER_COMMAND_POLL_INTERVAL_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	dataPlanePollMilliseconds, err := requiredPositiveInt64("SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	dataPlaneClaimMilliseconds, err := requiredPositiveInt64("SECONDBOX_DATA_PLANE_CLAIM_DURATION_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	dataPlaneRetentionSeconds, err := requiredPositiveInt64("SECONDBOX_DATA_PLANE_RETENTION_SECONDS")
	if err != nil {
		return Config{}, err
	}
	dataPlaneMaximumFrameBytes, err := requiredPositiveInt64("SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES")
	if err != nil {
		return Config{}, err
	}
	dataPlaneMaximumSessionBytes, err := requiredPositiveInt64("SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES")
	if err != nil {
		return Config{}, err
	}
	if dataPlaneMaximumSessionBytes < dataPlaneMaximumFrameBytes {
		return Config{}, errorsForEnvironment("data-plane session byte bound is smaller than frame byte bound")
	}
	lifecycleReconcilePollMilliseconds, err := requiredPositiveInt64("SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	lifecycleReconcileClaimMilliseconds, err := requiredPositiveInt64("SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	assignmentClaimMilliseconds, err := requiredPositiveInt64("SECONDBOX_ASSIGNMENT_CLAIM_DURATION_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	assignmentDeadlineMilliseconds, err := requiredPositiveInt64("SECONDBOX_ASSIGNMENT_DEADLINE_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	runnerHeartbeatTimeoutMilliseconds, err := requiredPositiveInt64("SECONDBOX_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	assignmentRetryLimit, err := requiredNonNegativeInt64("SECONDBOX_ASSIGNMENT_RETRY_LIMIT")
	if err != nil {
		return Config{}, err
	}
	schedulerSerializationRetryLimit, err := requiredNonNegativeInt64("SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT")
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
	objectStoreEndpoint, err := requiredString("SECONDBOX_OBJECT_STORE_ENDPOINT")
	if err != nil {
		return Config{}, err
	}
	objectStoreRegion, err := requiredString("SECONDBOX_OBJECT_STORE_REGION")
	if err != nil {
		return Config{}, err
	}
	objectStoreBucket, err := requiredString("SECONDBOX_OBJECT_STORE_BUCKET")
	if err != nil {
		return Config{}, err
	}
	objectStoreAccessKeyID, err := requiredString("SECONDBOX_OBJECT_STORE_ROOT_USER")
	if err != nil {
		return Config{}, err
	}
	objectStoreSecretAccessKey, err := requiredSecret("SECONDBOX_OBJECT_STORE_ROOT_PASSWORD", 24)
	if err != nil {
		return Config{}, err
	}
	objectStoreUsePathStyle, err := requiredBool("SECONDBOX_OBJECT_STORE_USE_PATH_STYLE")
	if err != nil {
		return Config{}, err
	}
	objectStoreRetryMaxAttempts, err := requiredPositiveInt64("SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS")
	if err != nil {
		return Config{}, err
	}
	objectStoreRetryMaxAttemptsInt := int(objectStoreRetryMaxAttempts)
	if int64(objectStoreRetryMaxAttemptsInt) != objectStoreRetryMaxAttempts {
		return Config{}, errorsForEnvironment("object store retry attempts exceed process integer range")
	}
	objectStoreHTTPTimeoutMilliseconds, err := requiredPositiveInt64("SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	objectStoreTempDirectory, err := requiredAbsolutePath("SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY")
	if err != nil {
		return Config{}, err
	}
	objectStoreMaxObjectBytes, err := requiredPositiveInt64("SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES")
	if err != nil {
		return Config{}, err
	}
	checkpointSpoolDirectory, err := requiredAbsolutePath("SECONDBOX_CHECKPOINT_SPOOL_DIRECTORY")
	if err != nil {
		return Config{}, err
	}
	runnerCredentialVerificationMilliseconds, err := requiredPositiveInt64("SECONDBOX_RUNNER_CREDENTIAL_VERIFICATION_TIMEOUT_MILLISECONDS")
	if err != nil {
		return Config{}, err
	}
	runnerProtocolMinimum, err := requiredUint32("SECONDBOX_RUNNER_PROTOCOL_MINIMUM")
	if err != nil {
		return Config{}, err
	}
	runnerProtocolMaximum, err := requiredUint32("SECONDBOX_RUNNER_PROTOCOL_MAXIMUM")
	if err != nil {
		return Config{}, err
	}
	if runnerProtocolMinimum > runnerProtocolMaximum {
		return Config{}, errorsForEnvironment("runner protocol minimum exceeds maximum")
	}
	runnerEnabledFeatures, err := requiredCSV("SECONDBOX_RUNNER_ENABLED_FEATURES")
	if err != nil {
		return Config{}, err
	}
	projectQuota, err := requiredQuota("SECONDBOX_DEFAULT_PROJECT")
	if err != nil {
		return Config{}, err
	}
	profileQuota, err := requiredQuota("SECONDBOX_DEFAULT_PROFILE")
	if err != nil {
		return Config{}, err
	}
	return Config{
		ListenAddress: listenAddress, PublicBaseURL: publicBaseURL, RunnerListenAddress: runnerListenAddress,
		DatabaseURL: databaseURL, LogPath: logPath,
		BootstrapAdminToken: bootstrapToken, APIKeyHashSecret: hashSecret,
		HTTPTimeout:                         time.Duration(httpSeconds) * time.Second,
		RunnerServerCertificatePath:         runnerServerCertificatePath,
		RunnerServerPrivateKeyPath:          runnerServerPrivateKeyPath,
		RunnerCACertificatePath:             runnerCACertificatePath,
		RunnerCAPrivateKeyPath:              runnerCAPrivateKeyPath,
		RunnerEnrollmentHashSecret:          runnerEnrollmentHashSecret,
		RunnerCertificateLifetime:           time.Duration(runnerCertificateLifetimeSeconds) * time.Second,
		RunnerCredentialVerificationTimeout: time.Duration(runnerCredentialVerificationMilliseconds) * time.Millisecond,
		RunnerHeartbeatInterval:             time.Duration(runnerHeartbeatMilliseconds) * time.Millisecond,
		RunnerCommandPollInterval:           time.Duration(runnerCommandPollMilliseconds) * time.Millisecond,
		DataPlanePollInterval:               time.Duration(dataPlanePollMilliseconds) * time.Millisecond,
		DataPlaneClaimDuration:              time.Duration(dataPlaneClaimMilliseconds) * time.Millisecond,
		DataPlaneRetention:                  time.Duration(dataPlaneRetentionSeconds) * time.Second,
		DataPlaneMaximumFrameBytes:          dataPlaneMaximumFrameBytes,
		DataPlaneMaximumSessionBytes:        dataPlaneMaximumSessionBytes,
		LifecycleReconcilePollInterval:      time.Duration(lifecycleReconcilePollMilliseconds) * time.Millisecond,
		LifecycleReconcileClaimDuration:     time.Duration(lifecycleReconcileClaimMilliseconds) * time.Millisecond,
		AssignmentClaimDuration:             time.Duration(assignmentClaimMilliseconds) * time.Millisecond,
		AssignmentDeadline:                  time.Duration(assignmentDeadlineMilliseconds) * time.Millisecond,
		RunnerHeartbeatTimeout:              time.Duration(runnerHeartbeatTimeoutMilliseconds) * time.Millisecond,
		AssignmentRetryLimit:                assignmentRetryLimit,
		SchedulerSerializationRetryLimit:    schedulerSerializationRetryLimitInt,
		SignedAssetCatalogPath:              signedAssetCatalogPath,
		ObjectStoreEndpoint:                 objectStoreEndpoint, ObjectStoreRegion: objectStoreRegion,
		ObjectStoreBucket: objectStoreBucket, ObjectStoreAccessKeyID: objectStoreAccessKeyID,
		ObjectStoreSecretAccessKey:  objectStoreSecretAccessKey,
		ObjectStoreUsePathStyle:     objectStoreUsePathStyle,
		ObjectStoreRetryMaxAttempts: objectStoreRetryMaxAttemptsInt,
		ObjectStoreHTTPTimeout:      time.Duration(objectStoreHTTPTimeoutMilliseconds) * time.Millisecond,
		ObjectStoreTempDirectory:    objectStoreTempDirectory,
		ObjectStoreMaxObjectBytes:   objectStoreMaxObjectBytes,
		CheckpointSpoolDirectory:    checkpointSpoolDirectory,
		RunnerProtocolMinimum:       runnerProtocolMinimum, RunnerProtocolMaximum: runnerProtocolMaximum,
		RunnerEnabledFeatures: runnerEnabledFeatures,
		DefaultProjectQuota:   projectQuota, DefaultProfileQuota: profileQuota,
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

func requiredUint32(name string) (uint32, error) {
	raw, err := requiredString(name)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("SecondBox environment variable %s must be a positive 32-bit integer", name)
	}
	return uint32(value), nil
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

func requiredBool(name string) (bool, error) {
	raw, err := requiredString(name)
	if err != nil {
		return false, err
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("SecondBox environment variable %s must be true or false", name)
	}
	return value, nil
}

func requiredQuota(prefix string) (contracts.QuotaLimits, error) {
	names := []string{
		"MAX_SANDBOXES", "MAX_ACTIVE_INSTANCES", "MAX_CPU_MILLIS", "MAX_MEMORY_BYTES",
		"MAX_RETAINED_BYTES", "MAX_SNAPSHOTS", "MAX_ARTIFACTS", "MAX_PORT_SESSIONS",
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
		MaxSandboxes: values[0], MaxActiveInstances: values[1], MaxCPUMillis: values[2],
		MaxMemoryBytes: values[3], MaxRetainedBytes: values[4], MaxSnapshots: values[5],
		MaxArtifacts: values[6], MaxPortSessions: values[7], MaxConcurrentOperations: values[8],
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

func errorsForEnvironment(message string) error {
	return fmt.Errorf("SecondBox environment configuration invalid: %s", message)
}
