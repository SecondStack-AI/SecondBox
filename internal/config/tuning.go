package config

import "strconv"

// TuningDefault describes one optional control-plane environment override.
// TOML field ownership lives in internal/deployconfig; this registry exposes
// only the process contract and its compiled value.
type TuningDefault struct {
	Environment string
	Value       string
}

// TuningDefaults returns the complete Category C surface in stable order.
func TuningDefaults() []TuningDefault {
	integer := func(name string, value int64) TuningDefault {
		return TuningDefault{Environment: name, Value: strconv.FormatInt(value, 10)}
	}
	return []TuningDefault{
		integer("SECONDBOX_HTTP_TIMEOUT_SECONDS", DefaultHTTPTimeoutSeconds),
		integer("SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS", DefaultRunnerHeartbeatIntervalMilliseconds),
		integer("SECONDBOX_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS", DefaultRunnerHeartbeatTimeoutMilliseconds),
		integer("SECONDBOX_RUNNER_COMMAND_DELIVERY_BATCH_SIZE", DefaultRunnerCommandDeliveryBatchSize),
		integer("SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_SIZE", DefaultRunnerEventPersistenceBatchSize),
		integer("SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_WAIT_MILLISECONDS", DefaultRunnerEventPersistenceBatchWaitMilliseconds),
		integer("SECONDBOX_DATA_PLANE_CLAIM_DURATION_MILLISECONDS", DefaultDataPlaneClaimDurationMilliseconds),
		integer("SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES", DefaultDataPlaneMaximumFrameBytes),
		integer("SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES", DefaultDataPlaneMaximumSessionBytes),
		integer("SECONDBOX_LIFECYCLE_RECONCILE_BATCH_SIZE", DefaultLifecycleReconcileBatchSize),
		integer("SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS", DefaultLifecycleReconcilePollIntervalMilliseconds),
		integer("SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS", DefaultLifecycleReconcileClaimDurationMilliseconds),
		integer("SECONDBOX_GARBAGE_COLLECTION_POLL_INTERVAL_MILLISECONDS", DefaultGarbageCollectionPollIntervalMilliseconds),
		integer("SECONDBOX_ASSIGNMENT_CLAIM_DURATION_MILLISECONDS", DefaultAssignmentClaimDurationMilliseconds),
		integer("SECONDBOX_ASSIGNMENT_DEADLINE_MILLISECONDS", DefaultAssignmentDeadlineMilliseconds),
		integer("SECONDBOX_ASSIGNMENT_RETRY_LIMIT", DefaultAssignmentRetryLimit),
		integer("SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT", DefaultSchedulerSerializationRetryLimit),
		integer("SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS", DefaultObjectStoreRetryMaxAttempts),
		integer("SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS", DefaultObjectStoreHTTPTimeoutMilliseconds),
		integer("SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES", DefaultObjectStoreMaxObjectBytes),
	}
}

func CategoryCEnvironmentNames() []string {
	defaults := TuningDefaults()
	names := make([]string, len(defaults))
	for index := range defaults {
		names[index] = defaults[index].Environment
	}
	return names
}
