// Package config loads required standalone SecondBox process configuration.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	DefaultLifecycleReconcileBatchSize                 int64 = 8
	DefaultLifecycleReconcilePollIntervalMilliseconds  int64 = 250
	DefaultLifecycleReconcileClaimDurationMilliseconds int64 = 30000
	DefaultGarbageCollectionPollIntervalMilliseconds   int64 = 60000
	DefaultAssignmentClaimDurationMilliseconds         int64 = 30000
	DefaultAssignmentDeadlineMilliseconds              int64 = 120000
	DefaultAssignmentRetryLimit                        int64 = 2
	DefaultSchedulerSerializationRetryLimit            int64 = 3
	DefaultObjectStoreRetryMaxAttempts                 int64 = 3
	DefaultObjectStoreHTTPTimeoutMilliseconds          int64 = 30000
	DefaultObjectStoreMaxObjectBytes                   int64 = 10737418240
)

// Config contains only explicitly configured control-plane settings.
type Config struct {
	ListenAddress                    string
	PublicBaseURL                    string
	RunnerListenAddress              string
	DatabaseURL                      string
	LogPath                          string
	PlatformToken                    string
	ApplicationAuthorities           []ApplicationAuthority
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
	LifecycleReconcileBatchSize      int
	LifecycleReconcilePollInterval   time.Duration
	LifecycleReconcileClaimDuration  time.Duration
	GarbageCollectionPollInterval    time.Duration
	AssignmentClaimDuration          time.Duration
	AssignmentDeadline               time.Duration
	RunnerHeartbeatTimeout           time.Duration
	AssignmentRetryLimit             int64
	SchedulerSerializationRetryLimit int
	SignedAssetCatalogPath           string
	ObjectStoreEndpoint              string
	ObjectStoreRegion                string
	ObjectStoreBucket                string
	ObjectStoreAccessKeyID           string
	ObjectStoreSecretAccessKey       string
	ObjectStoreUsePathStyle          bool
	ObjectStoreRetryMaxAttempts      int
	ObjectStoreHTTPTimeout           time.Duration
	ObjectStoreTempDirectory         string
	ObjectStoreMaxObjectBytes        int64
	RunnerEnabledFeatures            []string
	DefaultSubjectQuota              contracts.QuotaLimits
	AgentCompartmentProfile          BuiltInProfileBinding
	CodingEnvironmentProfile         BuiltInProfileBinding
}

// BuiltInProfileBinding is the deployment-specific RunnerPool and signed asset
// pair that one built-in Profile pins. SecondBox supplies no default for any of
// these values; a deployment names its own pool and its own verified bundles.
type BuiltInProfileBinding struct {
	Pool                  string
	RuntimeBundleDigest   string
	ToolchainBundleDigest string
}

// ApplicationAuthority configures one fixed application identity and its allowed capabilities.
type ApplicationAuthority struct {
	ID            string   `json:"id"`
	Token         string   `json:"token"`
	TenantRef     string   `json:"tenantRef"`
	SubjectRef    string   `json:"subjectRef"`
	Scopes        []string `json:"scopes"`
	ProfileGrants []string `json:"profileGrants"`
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
	applicationAuthorities, err := requiredApplicationAuthorities()
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
	garbageCollectionPollMilliseconds, err := optionalPositiveInt64("SECONDBOX_GARBAGE_COLLECTION_POLL_INTERVAL_MILLISECONDS", DefaultGarbageCollectionPollIntervalMilliseconds)
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
	objectStoreRetryMaxAttempts, err := optionalPositiveInt64("SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS", DefaultObjectStoreRetryMaxAttempts)
	if err != nil {
		return Config{}, err
	}
	objectStoreRetryMaxAttemptsInt := int(objectStoreRetryMaxAttempts)
	if int64(objectStoreRetryMaxAttemptsInt) != objectStoreRetryMaxAttempts {
		return Config{}, errorsForEnvironment("object store retry attempts exceed process integer range")
	}
	objectStoreHTTPTimeoutMilliseconds, err := optionalPositiveInt64("SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS", DefaultObjectStoreHTTPTimeoutMilliseconds)
	if err != nil {
		return Config{}, err
	}
	objectStoreTempDirectory, err := requiredAbsolutePath("SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY")
	if err != nil {
		return Config{}, err
	}
	objectStoreMaxObjectBytes, err := optionalPositiveInt64("SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES", DefaultObjectStoreMaxObjectBytes)
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
	agentCompartmentProfile, err := requiredBuiltInProfileBinding("AGENT_COMPARTMENT")
	if err != nil {
		return Config{}, err
	}
	codingEnvironmentProfile, err := requiredBuiltInProfileBinding("CODING_ENVIRONMENT")
	if err != nil {
		return Config{}, err
	}
	return Config{
		ListenAddress: listenAddress, PublicBaseURL: publicBaseURL, RunnerListenAddress: runnerListenAddress,
		DatabaseURL: databaseURL, LogPath: logPath,
		PlatformToken:                    platformToken,
		ApplicationAuthorities:           applicationAuthorities,
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
		LifecycleReconcileBatchSize:      lifecycleReconcileBatchSizeInt,
		LifecycleReconcilePollInterval:   time.Duration(lifecycleReconcilePollMilliseconds) * time.Millisecond,
		LifecycleReconcileClaimDuration:  time.Duration(lifecycleReconcileClaimMilliseconds) * time.Millisecond,
		GarbageCollectionPollInterval:    time.Duration(garbageCollectionPollMilliseconds) * time.Millisecond,
		AssignmentClaimDuration:          time.Duration(assignmentClaimMilliseconds) * time.Millisecond,
		AssignmentDeadline:               time.Duration(assignmentDeadlineMilliseconds) * time.Millisecond,
		RunnerHeartbeatTimeout:           time.Duration(runnerHeartbeatTimeoutMilliseconds) * time.Millisecond,
		AssignmentRetryLimit:             assignmentRetryLimit,
		SchedulerSerializationRetryLimit: schedulerSerializationRetryLimitInt,
		SignedAssetCatalogPath:           signedAssetCatalogPath,
		ObjectStoreEndpoint:              objectStoreEndpoint, ObjectStoreRegion: objectStoreRegion,
		ObjectStoreBucket: objectStoreBucket, ObjectStoreAccessKeyID: objectStoreAccessKeyID,
		ObjectStoreSecretAccessKey:  objectStoreSecretAccessKey,
		ObjectStoreUsePathStyle:     objectStoreUsePathStyle,
		ObjectStoreRetryMaxAttempts: objectStoreRetryMaxAttemptsInt,
		ObjectStoreHTTPTimeout:      time.Duration(objectStoreHTTPTimeoutMilliseconds) * time.Millisecond,
		ObjectStoreTempDirectory:    objectStoreTempDirectory,
		ObjectStoreMaxObjectBytes:   objectStoreMaxObjectBytes,
		RunnerEnabledFeatures:       runnerEnabledFeatures,
		DefaultSubjectQuota:         subjectQuota,
		AgentCompartmentProfile:     agentCompartmentProfile,
		CodingEnvironmentProfile:    codingEnvironmentProfile,
	}, nil
}

// requiredBuiltInProfileBinding reads the RunnerPool and signed bundle digests
// one built-in Profile pins. Every value is required and has no default.
func requiredBuiltInProfileBinding(profile string) (BuiltInProfileBinding, error) {
	prefix := "SECONDBOX_BUILTIN_" + profile + "_"
	pool, err := requiredString(prefix + "POOL")
	if err != nil {
		return BuiltInProfileBinding{}, err
	}
	runtimeDigest, err := requiredDigest(prefix + "RUNTIME_BUNDLE_DIGEST")
	if err != nil {
		return BuiltInProfileBinding{}, err
	}
	toolchainDigest, err := requiredDigest(prefix + "TOOLCHAIN_BUNDLE_DIGEST")
	if err != nil {
		return BuiltInProfileBinding{}, err
	}
	return BuiltInProfileBinding{
		Pool:                  pool,
		RuntimeBundleDigest:   runtimeDigest,
		ToolchainBundleDigest: toolchainDigest,
	}, nil
}

func requiredDigest(name string) (string, error) {
	value, err := requiredString(name)
	if err != nil {
		return "", err
	}
	if !sha256DigestPattern.MatchString(value) {
		return "", fmt.Errorf(
			"SecondBox environment variable %s must be a sha256:<64 hex characters> digest", name,
		)
	}
	return value, nil
}

var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func requiredApplicationAuthorities() ([]ApplicationAuthority, error) {
	raw, err := requiredString("SECONDBOX_APPLICATION_AUTHORITIES_JSON")
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var authorities []ApplicationAuthority
	if err := decoder.Decode(&authorities); err != nil {
		return nil, fmt.Errorf(
			"SecondBox environment variable SECONDBOX_APPLICATION_AUTHORITIES_JSON must be a JSON array: %w",
			err,
		)
	}
	if authorities == nil {
		return nil, errorsForEnvironment(
			"SECONDBOX_APPLICATION_AUTHORITIES_JSON must be an explicit JSON array",
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errorsForEnvironment(
			"SECONDBOX_APPLICATION_AUTHORITIES_JSON must contain a single JSON array",
		)
	}
	return authorities, nil
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
		"MAX_ARTIFACT_BYTES", "MAX_SNAPSHOTS", "MAX_ARTIFACTS", "MAX_PORT_SESSIONS",
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
		MaxMemoryBytes: values[3], MaxArtifactBytes: values[4], MaxSnapshots: values[5],
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
