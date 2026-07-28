// Package config loads required Sandbox Service process configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains only explicitly configured process settings.
type Config struct {
	ListenAddress        string
	DatabaseURL          string
	InternalToken        string
	AgentServiceURL      string
	AgentServiceToken    string
	SandboxHostURL       string
	SandboxHostToken     string
	HTTPTimeout          time.Duration
	LeaseTTL             time.Duration
	FileTransferMaxBytes int64
	ReconcileInterval    time.Duration
	ReconcileBatch       int
}

// FromEnvironment fails when any required configuration is absent or invalid.
func FromEnvironment() (Config, error) {
	listenAddress, err := required("SANDBOX_SERVICE_LISTEN_ADDR")
	if err != nil {
		return Config{}, err
	}
	databaseURL, err := required("SANDBOX_SERVICE_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	internalToken, err := required("SANDBOX_SERVICE_INTERNAL_TOKEN")
	if err != nil {
		return Config{}, err
	}
	agentServiceURL, err := required("SANDBOX_SERVICE_AGENT_SERVICE_URL")
	if err != nil {
		return Config{}, err
	}
	agentServiceToken, err := required("SANDBOX_SERVICE_AGENT_SERVICE_TOKEN")
	if err != nil {
		return Config{}, err
	}
	sandboxHostURL, err := required("SANDBOX_HOST_URL")
	if err != nil {
		return Config{}, err
	}
	sandboxHostToken, err := required("SANDBOX_HOST_TOKEN")
	if err != nil {
		return Config{}, err
	}
	httpTimeout, err := requiredDuration("SANDBOX_SERVICE_HTTP_TIMEOUT_SECONDS")
	if err != nil {
		return Config{}, err
	}
	leaseTTL, err := requiredDuration("SANDBOX_SERVICE_LEASE_TTL_SECONDS")
	if err != nil {
		return Config{}, err
	}
	fileTransferMaxBytes, err := requiredPositiveInt64("SANDBOX_SERVICE_FILE_TRANSFER_MAX_BYTES")
	if err != nil {
		return Config{}, err
	}
	reconcileInterval, err := requiredDuration("SANDBOX_SERVICE_RECONCILE_INTERVAL_SECONDS")
	if err != nil {
		return Config{}, err
	}
	reconcileBatch, err := requiredPositiveInt("SANDBOX_SERVICE_RECONCILE_BATCH_SIZE")
	if err != nil {
		return Config{}, err
	}
	if reconcileBatch > 1000 {
		return Config{}, fmt.Errorf("SANDBOX_SERVICE_RECONCILE_BATCH_SIZE must not exceed 1000")
	}
	return Config{
		ListenAddress: listenAddress, DatabaseURL: databaseURL, InternalToken: internalToken,
		AgentServiceURL: agentServiceURL, AgentServiceToken: agentServiceToken,
		SandboxHostURL: sandboxHostURL, SandboxHostToken: sandboxHostToken,
		HTTPTimeout: httpTimeout, LeaseTTL: leaseTTL, FileTransferMaxBytes: fileTransferMaxBytes,
		ReconcileInterval: reconcileInterval,
		ReconcileBatch:    reconcileBatch,
	}, nil
}

func required(name string) (string, error) {
	value, exists := os.LookupEnv(name)
	if !exists || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("missing required environment variable %s", name)
	}
	return value, nil
}

func requiredDuration(name string) (time.Duration, error) {
	value, err := requiredPositiveInt(name)
	if err != nil {
		return 0, err
	}
	return time.Duration(value) * time.Second, nil
}

func requiredPositiveInt(name string) (int, error) {
	raw, err := required(name)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func requiredPositiveInt64(name string) (int64, error) {
	raw, err := required(name)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}
