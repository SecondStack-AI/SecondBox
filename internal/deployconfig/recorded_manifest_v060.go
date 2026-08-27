package deployconfig

import (
	"bytes"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

const recordedManifestV060Version = "0.6.0"

// recordedManifestV060 is the exact deployment-manifest shape emitted by the
// v0.6.0 guided installer. It is accepted only after the update workflow has
// authenticated the recorded source release and only to recover the immutable
// Compose transport identity. Ordinary manifest decoding remains current and
// strict.
type recordedManifestV060 struct {
	SchemaVersion     int                           `toml:"schema_version"`
	Deployment        Deployment                    `toml:"deployment"`
	Database          Database                      `toml:"database"`
	RunnerTrust       RunnerTrust                   `toml:"runner_trust"`
	Runners           []Runner                      `toml:"runners"`
	Applications      Applications                  `toml:"applications"`
	Policy            recordedPolicyV060            `toml:"policy"`
	StandardResources recordedStandardResourcesV060 `toml:"standard_resources"`
	Overrides         TuningOverrides               `toml:"overrides"`
}

type recordedStandardResourcesV060 struct {
	ArtifactManifest string                   `toml:"artifact_manifest"`
	Bundles          []string                 `toml:"bundles"`
	RunnerPools      []recordedRunnerPoolV060 `toml:"runner_pools"`
	ApplyWaitSeconds *int64                   `toml:"apply_wait_seconds"`
}

type recordedRunnerPoolV060 struct {
	Bundle         string   `toml:"bundle"`
	Name           string   `toml:"name"`
	Architectures  []string `toml:"architectures"`
	Capabilities   []string `toml:"capabilities"`
	State          string   `toml:"state"`
	MaxSandboxes   *int64   `toml:"max_sandboxes"`
	MaxCPUMillis   *int64   `toml:"max_cpu_millis"`
	MaxMemoryBytes *int64   `toml:"max_memory_bytes"`
}

type recordedPolicyV060 struct {
	DataPlaneRetentionSeconds             *int64 `toml:"data_plane_retention_seconds"`
	DataPlanePollIntervalMilliseconds     *int64 `toml:"data_plane_poll_interval_milliseconds"`
	RunnerCommandPollIntervalMilliseconds *int64 `toml:"runner_command_poll_interval_milliseconds"`
	RunnerEnabledFeatures                 string `toml:"runner_enabled_features"`
	DefaultSubjectMaxSandboxes            *int64 `toml:"default_subject_max_sandboxes"`
	DefaultSubjectMaxActiveInstances      *int64 `toml:"default_subject_max_active_instances"`
	DefaultSubjectMaxCPUMillis            *int64 `toml:"default_subject_max_cpu_millis"`
	DefaultSubjectMaxMemoryBytes          *int64 `toml:"default_subject_max_memory_bytes"`
	DefaultSubjectMaxSnapshots            *int64 `toml:"default_subject_max_snapshots"`
	DefaultSubjectMaxPortSessions         *int64 `toml:"default_subject_max_port_sessions"`
	DefaultSubjectMaxConcurrentOperations *int64 `toml:"default_subject_max_concurrent_operations"`
}

func readRecordedManifestV060(path string) (recordedManifestV060, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return recordedManifestV060{}, manifestError("open recorded v0.6.0 manifest", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return recordedManifestV060{}, manifestError("recorded v0.6.0 manifest path must be a regular non-symbolic-link file", nil)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return recordedManifestV060{}, manifestError("read recorded v0.6.0 manifest", err)
	}
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest recordedManifestV060
	if err := decoder.Decode(&manifest); err != nil {
		return recordedManifestV060{}, manifestError("strict decode recorded v0.6.0 manifest", err)
	}
	if manifest.SchemaVersion != 1 {
		return recordedManifestV060{}, manifestError(fmt.Sprintf("unsupported recorded v0.6.0 schema_version %d", manifest.SchemaVersion), nil)
	}
	return manifest, nil
}
