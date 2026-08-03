package deployconfig

import (
	"fmt"
	"reflect"

	controlconfig "github.com/SecondStack-AI/SecondBox/internal/config"
)

type OverrideDefinition struct {
	TOMLName    string
	Environment string
	Default     string
	Help        string
	AllowZero   bool
	field       string
}

// OverrideRegistry is the single discoverability and mapping registry for all
// optional control-plane tuning values.
func OverrideRegistry() []OverrideDefinition {
	defaults := make(map[string]string)
	for _, item := range controlconfig.TuningDefaults() {
		defaults[item.Environment] = item.Value
	}
	definitions := []OverrideDefinition{
		{"http_timeout_seconds", "SECONDBOX_HTTP_TIMEOUT_SECONDS", "", "HTTP request timeout.", false, "HTTPTimeoutSeconds"},
		{"runner_heartbeat_interval_milliseconds", "SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS", "", "Runner heartbeat cadence.", false, "RunnerHeartbeatIntervalMilliseconds"},
		{"runner_heartbeat_timeout_milliseconds", "SECONDBOX_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS", "", "Runner liveness timeout.", false, "RunnerHeartbeatTimeoutMilliseconds"},
		{"runner_command_delivery_batch_size", "SECONDBOX_RUNNER_COMMAND_DELIVERY_BATCH_SIZE", "", "Commands delivered per claim.", false, "RunnerCommandDeliveryBatchSize"},
		{"runner_event_persistence_batch_size", "SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_SIZE", "", "Runner events persisted per batch.", false, "RunnerEventPersistenceBatchSize"},
		{"runner_event_persistence_batch_wait_milliseconds", "SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_WAIT_MILLISECONDS", "", "Maximum runner-event batch wait.", false, "RunnerEventPersistenceBatchWaitMilliseconds"},
		{"data_plane_claim_duration_milliseconds", "SECONDBOX_DATA_PLANE_CLAIM_DURATION_MILLISECONDS", "", "Relay frame delivery claim duration.", false, "DataPlaneClaimDurationMilliseconds"},
		{"data_plane_maximum_frame_bytes", "SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES", "", "Maximum relay frame payload.", false, "DataPlaneMaximumFrameBytes"},
		{"data_plane_maximum_session_bytes", "SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES", "", "Maximum relay session payload.", false, "DataPlaneMaximumSessionBytes"},
		{"lifecycle_reconcile_batch_size", "SECONDBOX_LIFECYCLE_RECONCILE_BATCH_SIZE", "", "Lifecycle rows reconciled per batch.", false, "LifecycleReconcileBatchSize"},
		{"lifecycle_reconcile_poll_interval_milliseconds", "SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS", "", "Lifecycle fallback polling cadence.", false, "LifecycleReconcilePollIntervalMilliseconds"},
		{"lifecycle_reconcile_claim_duration_milliseconds", "SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS", "", "Lifecycle work claim duration.", false, "LifecycleReconcileClaimDurationMilliseconds"},
		{"garbage_collection_poll_interval_milliseconds", "SECONDBOX_GARBAGE_COLLECTION_POLL_INTERVAL_MILLISECONDS", "", "Artifact garbage-collection polling cadence.", false, "GarbageCollectionPollIntervalMilliseconds"},
		{"assignment_claim_duration_milliseconds", "SECONDBOX_ASSIGNMENT_CLAIM_DURATION_MILLISECONDS", "", "Assignment dispatch claim duration.", false, "AssignmentClaimDurationMilliseconds"},
		{"assignment_deadline_milliseconds", "SECONDBOX_ASSIGNMENT_DEADLINE_MILLISECONDS", "", "Assignment readiness deadline.", false, "AssignmentDeadlineMilliseconds"},
		{"assignment_retry_limit", "SECONDBOX_ASSIGNMENT_RETRY_LIMIT", "", "Assignment retry count.", true, "AssignmentRetryLimit"},
		{"scheduler_serialization_retry_limit", "SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT", "", "Serializable scheduling retry count.", true, "SchedulerSerializationRetryLimit"},
		{"object_store_retry_max_attempts", "SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS", "", "Object-store operation attempts.", false, "ObjectStoreRetryMaxAttempts"},
		{"object_store_http_timeout_milliseconds", "SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS", "", "Object-store HTTP timeout.", false, "ObjectStoreHTTPTimeoutMilliseconds"},
		{"object_store_max_object_bytes", "SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES", "", "Maximum object size accepted by the adapter.", false, "ObjectStoreMaxObjectBytes"},
	}
	for index := range definitions {
		definitions[index].Default = defaults[definitions[index].Environment]
	}
	return definitions
}

func resolvedOverrides(overrides TuningOverrides) (map[string]string, error) {
	values := make(map[string]string)
	value := reflect.ValueOf(overrides)
	for _, definition := range OverrideRegistry() {
		field := value.FieldByName(definition.field)
		if !field.IsValid() {
			return nil, fmt.Errorf("SecondBox deployment manifest internal override mapping missing %s", definition.TOMLName)
		}
		if field.IsNil() {
			continue
		}
		integer := field.Elem().Int()
		if integer < 0 || (!definition.AllowZero && integer == 0) {
			return nil, fmt.Errorf("SecondBox deployment manifest overrides.%s must be %s", definition.TOMLName, map[bool]string{true: "non-negative", false: "positive"}[definition.AllowZero])
		}
		values[definition.Environment] = fmt.Sprintf("%d", integer)
	}
	return values, nil
}
