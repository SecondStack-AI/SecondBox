#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
  echo "Usage: deploy/bin/validate-environment.sh PATH" >&2
  exit 2
fi

environment_path="$1"
if [[ -L "$environment_path" || ! -f "$environment_path" ]]; then
  echo "SecondBox environment must be a regular non-symbolic-link file: $environment_path" >&2
  exit 1
fi
for required_command in awk cmp grep openssl stat; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "Environment validation requires command: $required_command" >&2
    exit 1
  fi
done

file_mode="$(stat -c '%a' "$environment_path")"
if (( (8#$file_mode & 8#077) != 0 )); then
  echo "SecondBox environment permissions must not grant group or other access: $environment_path" >&2
  exit 1
fi

value_for() {
  local key="$1"
  awk -F= -v key="$key" '
    $1 == key {
      sub(/^[^=]*=/, "")
      print
      found++
    }
    END {
      if (found != 1) {
        exit 1
      }
    }
  ' "$environment_path"
}

required_settings=(
  SECONDBOX_DEPLOYMENT_MODE
  SECONDBOX_PUBLIC_BASE_URL
  SECONDBOX_TLS_TERMINATION
  SECONDBOX_CONTROL_PLANE_IMAGE
  SECONDBOX_RUNNER_IMAGE
  SECONDBOX_POSTGRES_IMAGE
  SECONDBOX_OBJECT_STORE_IMAGE
  SECONDBOX_OBJECT_STORE_CLIENT_IMAGE
  SECONDBOX_API_BIND_IP
  SECONDBOX_API_PUBLISHED_PORT
  SECONDBOX_LISTEN_ADDR
  SECONDBOX_LOG_PATH
  SECONDBOX_HTTP_TIMEOUT_SECONDS
  SECONDBOX_RUNNER_BIND_IP
  SECONDBOX_RUNNER_PUBLISHED_PORT
  SECONDBOX_RUNNER_LISTEN_ADDR
  SECONDBOX_RUNNER_SERVER_NAME
  SECONDBOX_RUNNER_PKI_HOST_DIR
  SECONDBOX_RUNNER_SERVER_CERTIFICATE
  SECONDBOX_RUNNER_SERVER_PRIVATE_KEY
  SECONDBOX_RUNNER_CA_CERTIFICATE
  SECONDBOX_RUNNER_CA_PRIVATE_KEY
  SECONDBOX_RUNNER_CREDENTIAL
  SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS
  SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS
  SECONDBOX_RUNNER_COMMAND_POLL_INTERVAL_MILLISECONDS
  SECONDBOX_RUNNER_COMMAND_DELIVERY_BATCH_SIZE
  SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_SIZE
  SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_WAIT_MILLISECONDS
  SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS
  SECONDBOX_DATA_PLANE_CLAIM_DURATION_MILLISECONDS
  SECONDBOX_DATA_PLANE_RETENTION_SECONDS
  SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES
  SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES
  SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS
  SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS
  SECONDBOX_ASSIGNMENT_CLAIM_DURATION_MILLISECONDS
  SECONDBOX_ASSIGNMENT_DEADLINE_MILLISECONDS
  SECONDBOX_ASSIGNMENT_RETRY_LIMIT
  SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT
  SECONDBOX_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS
  SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH
  SECONDBOX_SIGNED_ASSET_CATALOG_PATH
  SECONDBOX_BUILTIN_AGENT_COMPARTMENT_POOL
  SECONDBOX_BUILTIN_AGENT_COMPARTMENT_RUNTIME_BUNDLE_DIGEST
  SECONDBOX_BUILTIN_AGENT_COMPARTMENT_TOOLCHAIN_BUNDLE_DIGEST
  SECONDBOX_BUILTIN_CODING_ENVIRONMENT_POOL
  SECONDBOX_BUILTIN_CODING_ENVIRONMENT_RUNTIME_BUNDLE_DIGEST
  SECONDBOX_BUILTIN_CODING_ENVIRONMENT_TOOLCHAIN_BUNDLE_DIGEST
  SECONDBOX_RUNNER_PROTOCOL_MINIMUM
  SECONDBOX_RUNNER_PROTOCOL_MAXIMUM
  SECONDBOX_RUNNER_ENABLED_FEATURES
  SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT
  SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT
  SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL
  SECONDBOX_SAME_HOST_RUNNER_ENABLED
  SECONDBOX_RUNNER_ID
  SECONDBOX_RUNNER_POOL_ID
  SECONDBOX_RUNNER_SOFTWARE_VERSION
  SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS
  SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME
  SECONDBOX_RUNNER_IDENTITY_HOST_DIR
  SECONDBOX_RUNNER_ARTIFACT_HOST_DIR
  SECONDBOX_RUNNER_STATE_HOST_DIR
  SECONDBOX_RUNNER_WORKSPACE_HOST_DIR
  SECONDBOX_RUNNER_LOG_PATH
  SECONDBOX_RUNNER_LOG_DIR
  SECONDBOX_RUNNER_FIRECRACKER_PATH
  SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH
  SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT
  SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID
  SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID
  SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION
  SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT
  SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH
  SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH
  SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH
  SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS
  SECONDBOX_RUNNER_FIRECRACKER_CPU_TEMPLATE
  SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR
  SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR
  SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256
  SECONDBOX_RUNNER_WORKSPACE_ROOT
  SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT
  SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT
  SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT
  SECONDBOX_RUNNER_SANDBOX_MAX_VCPUS
  SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB
  SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB
  SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB
  SECONDBOX_RUNNER_SANDBOX_GUEST_IP
  SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME
  SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR
  SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR
  SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX
  SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR
  SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE
  SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH
  SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS
  SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL
  SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES
  SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS
  SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS
  SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM
  SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX
  SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL
  SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS
  SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES
  SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL
  SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES
  SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS
  SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS
  SECONDBOX_POSTGRES_BIND_IP
  SECONDBOX_POSTGRES_PUBLISHED_PORT
  SECONDBOX_POSTGRES_DATABASE
  SECONDBOX_POSTGRES_USER
  SECONDBOX_POSTGRES_PASSWORD
  SECONDBOX_DATABASE_URL
  SECONDBOX_OBJECT_STORE_BIND_IP
  SECONDBOX_OBJECT_STORE_PUBLISHED_PORT
  SECONDBOX_OBJECT_STORE_CONSOLE_PUBLISHED_PORT
  SECONDBOX_OBJECT_STORE_ENDPOINT
  SECONDBOX_OBJECT_STORE_BUCKET
  SECONDBOX_OBJECT_STORE_REGION
  SECONDBOX_OBJECT_STORE_ROOT_USER
  SECONDBOX_OBJECT_STORE_ROOT_PASSWORD
  SECONDBOX_OBJECT_STORE_USE_PATH_STYLE
  SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS
  SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS
  SECONDBOX_DEVELOPMENT_PREPARE_WAIT_TIMEOUT_SECONDS
  SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY
  SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES
  SECONDBOX_PLATFORM_TOKEN
  SECONDBOX_APPLICATION_AUTHORITIES_JSON
  SECONDBOX_DEFAULT_SUBJECT_MAX_SANDBOXES
  SECONDBOX_DEFAULT_SUBJECT_MAX_ACTIVE_INSTANCES
  SECONDBOX_DEFAULT_SUBJECT_MAX_CPU_MILLIS
  SECONDBOX_DEFAULT_SUBJECT_MAX_MEMORY_BYTES
  SECONDBOX_DEFAULT_SUBJECT_MAX_ARTIFACT_BYTES
  SECONDBOX_DEFAULT_SUBJECT_MAX_SNAPSHOTS
  SECONDBOX_DEFAULT_SUBJECT_MAX_ARTIFACTS
  SECONDBOX_DEFAULT_SUBJECT_MAX_PORT_SESSIONS
  SECONDBOX_DEFAULT_SUBJECT_MAX_CONCURRENT_OPERATIONS
)

for setting in "${required_settings[@]}"; do
  if ! value="$(value_for "$setting")" || [[ -z "$value" ]]; then
    echo "SecondBox environment requires exactly one non-empty $setting" >&2
    exit 1
  fi
  if [[ "$value" == *GENERATE_WITH_DEPLOY_BOOTSTRAP* ||
        "$value" == *GENERATE_LOCAL_DATABASE_URL* ||
        "$value" == *GENERATE_DEVELOPMENT_BUNDLE_DIGEST* ||
        "$value" == *REPLACE_WITH_* ]]; then
    echo "SecondBox environment still contains a placeholder for $setting" >&2
    exit 1
  fi
done

platform_token="$(value_for SECONDBOX_PLATFORM_TOKEN)"
postgres_password="$(value_for SECONDBOX_POSTGRES_PASSWORD)"
object_store_password="$(value_for SECONDBOX_OBJECT_STORE_ROOT_PASSWORD)"
runner_credential="$(value_for SECONDBOX_RUNNER_CREDENTIAL)"
for secret_setting in \
  SECONDBOX_PLATFORM_TOKEN \
  SECONDBOX_POSTGRES_PASSWORD \
  SECONDBOX_OBJECT_STORE_ROOT_PASSWORD \
  SECONDBOX_RUNNER_CREDENTIAL; do
  secret_value="$(value_for "$secret_setting")"
  if (( ${#secret_value} < 24 )); then
    echo "$secret_setting must contain at least 24 bytes" >&2
    exit 1
  fi
done
if (( ${#runner_credential} < 32 )); then
  echo "SECONDBOX_RUNNER_CREDENTIAL must contain at least 32 bytes" >&2
  exit 1
fi
if [[ "$platform_token" == "$postgres_password" ||
      "$platform_token" == "$object_store_password" ||
      "$platform_token" == "$runner_credential" ||
      "$postgres_password" == "$object_store_password" ||
      "$postgres_password" == "$runner_credential" ||
      "$object_store_password" == "$runner_credential" ]]; then
  echo "SecondBox deployment credentials must be unique per trust boundary" >&2
  exit 1
fi

for port_setting in \
  SECONDBOX_API_PUBLISHED_PORT \
  SECONDBOX_RUNNER_PUBLISHED_PORT \
  SECONDBOX_POSTGRES_PUBLISHED_PORT \
  SECONDBOX_OBJECT_STORE_PUBLISHED_PORT \
  SECONDBOX_OBJECT_STORE_CONSOLE_PUBLISHED_PORT \
  SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT \
  SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT; do
  port="$(value_for "$port_setting")"
  if [[ ! "$port" =~ ^[0-9]+$ ]] || (( port < 1 || port > 65535 )); then
    echo "$port_setting must be an integer from 1 through 65535" >&2
    exit 1
  fi
done
for data_plane_setting in \
  SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS \
  SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS; do
  data_plane_address="$(value_for "$data_plane_setting")"
  data_plane_port="${data_plane_address##*:}"
  if [[ "$data_plane_address" != *:* ]] ||
     [[ ! "$data_plane_port" =~ ^[0-9]+$ ]] ||
     (( data_plane_port < 1 || data_plane_port > 65535 )); then
    echo "$data_plane_setting must be a host:port address with a port from 1 through 65535" >&2
    exit 1
  fi
done
# The advertised address is dialed by the ingress tier, so a wildcard host is a
# configuration error even though it is a valid bind host.
data_plane_advertised_address="$(value_for SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS)"
data_plane_advertised_host="${data_plane_advertised_address%:*}"
if [[ -z "$data_plane_advertised_host" ||
      "$data_plane_advertised_host" == "0.0.0.0" ||
      "$data_plane_advertised_host" == "[::]" ]]; then
  echo "SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS must name a reachable host, not a wildcard" >&2
  exit 1
fi
guest_control_vsock_port="$(value_for SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT)"
guest_protocol_vsock_port="$(value_for SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT)"
if [[ "$guest_control_vsock_port" == "$guest_protocol_vsock_port" ]]; then
  echo "SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT and SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT must be distinct" >&2
  exit 1
fi

duration_nanoseconds() {
  awk -v duration="$1" '
    BEGIN {
      remaining = duration
      total = 0
      if (remaining == "" || remaining ~ /^[+-]/) {
        exit 1
      }
      while (remaining != "") {
        if (match(remaining, /^[0-9]+([.][0-9]+)?/) != 1) {
          exit 1
        }
        number = substr(remaining, 1, RLENGTH) + 0
        remaining = substr(remaining, RLENGTH + 1)
        if (substr(remaining, 1, 2) == "ns") {
          multiplier = 1
          remaining = substr(remaining, 3)
        } else if (substr(remaining, 1, 2) == "us") {
          multiplier = 1000
          remaining = substr(remaining, 3)
        } else if (substr(remaining, 1, 2) == "ms") {
          multiplier = 1000000
          remaining = substr(remaining, 3)
        } else if (substr(remaining, 1, 1) == "s") {
          multiplier = 1000000000
          remaining = substr(remaining, 2)
        } else if (substr(remaining, 1, 1) == "m") {
          multiplier = 60000000000
          remaining = substr(remaining, 2)
        } else if (substr(remaining, 1, 1) == "h") {
          multiplier = 3600000000000
          remaining = substr(remaining, 2)
        } else {
          exit 1
        }
        total += number * multiplier
      }
      printf "%.0f\n", total
    }
  '
}

guest_heartbeat_interval="$(value_for SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL)"
guest_heartbeat_nanoseconds="$(duration_nanoseconds "$guest_heartbeat_interval")" || {
  echo "SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL must be a positive Go duration from 1ms through 60s" >&2
  exit 1
}
if [[ ! "$guest_heartbeat_nanoseconds" =~ ^[0-9]+$ ]] ||
   (( guest_heartbeat_nanoseconds < 1000000 || guest_heartbeat_nanoseconds > 60000000000 )); then
  echo "SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL must be a positive Go duration from 1ms through 60s" >&2
  exit 1
fi
network_policy_dns_ttl="$(value_for SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL)"
network_policy_dns_ttl_nanoseconds="$(duration_nanoseconds "$network_policy_dns_ttl")" || {
  echo "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL must be a positive Go duration" >&2
  exit 1
}
if [[ ! "$network_policy_dns_ttl_nanoseconds" =~ ^[0-9]+$ ]] ||
   (( network_policy_dns_ttl_nanoseconds < 1 )); then
  echo "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL must be a positive Go duration" >&2
  exit 1
fi

for positive_setting in \
  SECONDBOX_HTTP_TIMEOUT_SECONDS \
  SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS \
  SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS \
  SECONDBOX_RUNNER_COMMAND_POLL_INTERVAL_MILLISECONDS \
  SECONDBOX_RUNNER_COMMAND_DELIVERY_BATCH_SIZE \
  SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_SIZE \
  SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_WAIT_MILLISECONDS \
  SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS \
  SECONDBOX_DATA_PLANE_CLAIM_DURATION_MILLISECONDS \
  SECONDBOX_DATA_PLANE_RETENTION_SECONDS \
  SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES \
  SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES \
  SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS \
  SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS \
  SECONDBOX_ASSIGNMENT_CLAIM_DURATION_MILLISECONDS \
  SECONDBOX_ASSIGNMENT_DEADLINE_MILLISECONDS \
  SECONDBOX_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS \
  SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS \
  SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS \
  SECONDBOX_DEVELOPMENT_PREPARE_WAIT_TIMEOUT_SECONDS \
  SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES \
  SECONDBOX_RUNNER_PROTOCOL_MINIMUM \
  SECONDBOX_RUNNER_PROTOCOL_MAXIMUM; do
  positive_value="$(value_for "$positive_setting")"
  if [[ ! "$positive_value" =~ ^[0-9]+$ ]] || (( positive_value < 1 )); then
    echo "$positive_setting must be a positive integer" >&2
    exit 1
  fi
done
data_plane_frame_bytes="$(value_for SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES)"
data_plane_session_bytes="$(value_for SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES)"
if (( data_plane_session_bytes < data_plane_frame_bytes )); then
  echo "SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES must be at least SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES" >&2
  exit 1
fi
if [[ "$(value_for SECONDBOX_OBJECT_STORE_USE_PATH_STYLE)" != "true" &&
      "$(value_for SECONDBOX_OBJECT_STORE_USE_PATH_STYLE)" != "false" ]]; then
  echo "SECONDBOX_OBJECT_STORE_USE_PATH_STYLE must be true or false" >&2
  exit 1
fi
for absolute_directory_setting in \
  SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY; do
  absolute_directory="$(value_for "$absolute_directory_setting")"
  if [[ "$absolute_directory" != /* ]]; then
    echo "$absolute_directory_setting must be absolute" >&2
    exit 1
  fi
done
for retry_setting in \
  SECONDBOX_ASSIGNMENT_RETRY_LIMIT \
  SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT; do
  retry_value="$(value_for "$retry_setting")"
  if [[ ! "$retry_value" =~ ^[0-9]+$ ]]; then
    echo "$retry_setting must be a non-negative integer" >&2
    exit 1
  fi
done
if (( $(value_for SECONDBOX_RUNNER_PROTOCOL_MINIMUM) > $(value_for SECONDBOX_RUNNER_PROTOCOL_MAXIMUM) )); then
  echo "SECONDBOX_RUNNER_PROTOCOL_MINIMUM must not exceed SECONDBOX_RUNNER_PROTOCOL_MAXIMUM" >&2
  exit 1
fi

runner_pki_directory="$(value_for SECONDBOX_RUNNER_PKI_HOST_DIR)"
if [[ "$runner_pki_directory" != /* ||
      -L "$runner_pki_directory" ||
      ! -d "$runner_pki_directory" ]]; then
  echo "SECONDBOX_RUNNER_PKI_HOST_DIR must be an absolute non-symbolic-link directory" >&2
  exit 1
fi

same_host_runner_enabled="$(value_for SECONDBOX_SAME_HOST_RUNNER_ENABLED)"
if [[ "$same_host_runner_enabled" != "true" && "$same_host_runner_enabled" != "false" ]]; then
  echo "SECONDBOX_SAME_HOST_RUNNER_ENABLED must be true or false" >&2
  exit 1
fi
if [[ "$(value_for SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED)" != "false" ]]; then
  echo "SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED must be false for the packaged Runner" >&2
  exit 1
fi
for runner_positive_setting in \
  SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID \
  SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID \
  SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION \
  SECONDBOX_RUNNER_SANDBOX_MAX_VCPUS \
  SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB \
  SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB \
  SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT \
  SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT \
  SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT \
  SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB \
  SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS \
  SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX \
  SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL \
  SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS \
  SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES \
  SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL \
  SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES; do
  runner_positive_value="$(value_for "$runner_positive_setting")"
  if [[ ! "$runner_positive_value" =~ ^[0-9]+$ ]] || (( runner_positive_value < 1 )); then
    echo "$runner_positive_setting must be a positive integer" >&2
    exit 1
  fi
done
runner_max_concurrent_starts="$(value_for SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS)"
runner_max_concurrent_global="$(value_for SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL)"
if (( runner_max_concurrent_starts > runner_max_concurrent_global )); then
  echo "SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS must not exceed SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL" >&2
  exit 1
fi
runner_storage_recovery="$(value_for SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT)"
runner_storage_warning="$(value_for SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT)"
runner_storage_deny="$(value_for SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT)"
if (( runner_storage_recovery >= runner_storage_warning ||
      runner_storage_warning >= runner_storage_deny ||
      runner_storage_deny >= 100 )); then
  echo "Runner storage pressure thresholds must satisfy 0 < recovery < warning < admission deny < 100" >&2
  exit 1
fi
if [[ "$same_host_runner_enabled" == "true" ]]; then
  for runner_host_directory_setting in \
    SECONDBOX_RUNNER_IDENTITY_HOST_DIR \
    SECONDBOX_RUNNER_ARTIFACT_HOST_DIR \
    SECONDBOX_RUNNER_STATE_HOST_DIR \
    SECONDBOX_RUNNER_WORKSPACE_HOST_DIR; do
    runner_host_directory="$(value_for "$runner_host_directory_setting")"
    if [[ "$runner_host_directory" != /* || -L "$runner_host_directory" || ! -d "$runner_host_directory" ]]; then
      echo "$runner_host_directory_setting must be an existing absolute non-symbolic-link directory" >&2
      exit 1
    fi
  done
  runner_root_device="$(stat -c '%d' /)"
  runner_workspace_device="$(stat -c '%d' "$(value_for SECONDBOX_RUNNER_WORKSPACE_HOST_DIR)")"
  if [[ "$runner_workspace_device" == "$runner_root_device" ]]; then
    echo "Runner workspace root must use a dedicated non-root filesystem" >&2
    exit 1
  fi
  for runner_identity_file in runner.crt runner.key runner-ca.crt; do
    runner_identity_path="$(value_for SECONDBOX_RUNNER_IDENTITY_HOST_DIR)/$runner_identity_file"
    if [[ -L "$runner_identity_path" || ! -f "$runner_identity_path" ]]; then
      echo "Same-host Runner identity file is missing: $runner_identity_path" >&2
      exit 1
    fi
  done
  runner_identity_key_mode="$(stat -c '%a' "$(value_for SECONDBOX_RUNNER_IDENTITY_HOST_DIR)/runner.key")"
  if (( (8#$runner_identity_key_mode & 8#077) != 0 )); then
    echo "Same-host Runner private key must not grant group or other access" >&2
    exit 1
  fi
  runner_identity_directory="$(value_for SECONDBOX_RUNNER_IDENTITY_HOST_DIR)"
  if ! cmp -s "$runner_identity_directory/runner-ca.crt" "$runner_pki_directory/runner-ca.crt"; then
    echo "Same-host Runner identity must trust the configured control-plane Runner CA" >&2
    exit 1
  fi
  if ! openssl verify \
    -CAfile "$runner_identity_directory/runner-ca.crt" \
    "$runner_identity_directory/runner.crt" >/dev/null; then
    echo "Same-host Runner certificate is not signed by the configured Runner CA" >&2
    exit 1
  fi
  runner_identity_certificate_public_key="$(
    openssl x509 -in "$runner_identity_directory/runner.crt" -pubkey -noout
  )"
  runner_identity_private_public_key="$(
    openssl pkey -in "$runner_identity_directory/runner.key" -pubout
  )"
  if [[ "$runner_identity_certificate_public_key" != "$runner_identity_private_public_key" ]]; then
    echo "Same-host Runner certificate and private key do not match" >&2
    exit 1
  fi
  if [[ ! "$(value_for SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256)" =~ ^[0-9a-f]{64}$ ||
        "$(value_for SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256)" == "0000000000000000000000000000000000000000000000000000000000000000" ]]; then
    echo "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256 must identify a provisioned signed artifact key" >&2
    exit 1
  fi
fi

signed_asset_catalog_host_path="$(value_for SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH)"
signed_asset_catalog_path="$(value_for SECONDBOX_SIGNED_ASSET_CATALOG_PATH)"
if [[ "$signed_asset_catalog_host_path" != /* ||
      -L "$signed_asset_catalog_host_path" ||
      ! -f "$signed_asset_catalog_host_path" ]]; then
  echo "SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH must be an existing absolute non-symbolic-link file" >&2
  exit 1
fi
if [[ "$signed_asset_catalog_path" != /* ]]; then
  echo "SECONDBOX_SIGNED_ASSET_CATALOG_PATH must be absolute" >&2
  exit 1
fi

for private_key in runner-ca.key server.key; do
  private_key_path="$runner_pki_directory/$private_key"
  if [[ -L "$private_key_path" || ! -f "$private_key_path" ]]; then
    echo "Runner PKI private key must be a regular non-symbolic-link file: $private_key_path" >&2
    exit 1
  fi
  private_key_mode="$(stat -c '%a' "$private_key_path")"
  if (( (8#$private_key_mode & 8#077) != 0 )); then
    echo "Runner PKI private key must not grant group or other access: $private_key_path" >&2
    exit 1
  fi
done
for certificate in runner-ca.crt server.crt; do
  certificate_path="$runner_pki_directory/$certificate"
  if [[ -L "$certificate_path" || ! -f "$certificate_path" ]]; then
    echo "Runner PKI certificate must be a regular non-symbolic-link file: $certificate_path" >&2
    exit 1
  fi
done
for setting_and_path in \
  "SECONDBOX_RUNNER_SERVER_CERTIFICATE=/run/secondbox-runner-pki/server.crt" \
  "SECONDBOX_RUNNER_SERVER_PRIVATE_KEY=/run/secondbox-runner-pki/server.key" \
  "SECONDBOX_RUNNER_CA_CERTIFICATE=/run/secondbox-runner-pki/runner-ca.crt"; do
  setting="${setting_and_path%%=*}"
  expected_path="${setting_and_path#*=}"
  if [[ "$(value_for "$setting")" != "$expected_path" ]]; then
    echo "$setting must be $expected_path for the packaged Compose mount" >&2
    exit 1
  fi
done
if [[ "$(value_for SECONDBOX_RUNNER_CA_PRIVATE_KEY)" != "$runner_pki_directory/runner-ca.key" ]]; then
  echo "SECONDBOX_RUNNER_CA_PRIVATE_KEY must identify the private runner CA in SECONDBOX_RUNNER_PKI_HOST_DIR" >&2
  exit 1
fi
if ! openssl verify \
  -CAfile "$runner_pki_directory/runner-ca.crt" \
  "$runner_pki_directory/server.crt" >/dev/null; then
  echo "Runner server certificate is not signed by the configured runner CA" >&2
  exit 1
fi
runner_server_name="$(value_for SECONDBOX_RUNNER_SERVER_NAME)"
runner_server_check_option="-checkhost"
if [[ "$runner_server_name" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  IFS='.' read -r -a runner_server_octets <<<"$runner_server_name"
  for octet in "${runner_server_octets[@]}"; do
    if (( 10#$octet > 255 )); then
      echo "SECONDBOX_RUNNER_SERVER_NAME must be a valid DNS name or IPv4 address" >&2
      exit 1
    fi
  done
  runner_server_check_option="-checkip"
elif (( ${#runner_server_name} <= 253 )) &&
     [[ "$runner_server_name" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]$ ||
        "$runner_server_name" =~ ^[A-Za-z0-9]$ ]] &&
     [[ "$runner_server_name" != *..* ]]; then
  IFS='.' read -r -a runner_server_labels <<<"$runner_server_name"
  for label in "${runner_server_labels[@]}"; do
    if (( ${#label} > 63 )) ||
       [[ ! "$label" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$ &&
          ! "$label" =~ ^[A-Za-z0-9]$ ]]; then
      echo "SECONDBOX_RUNNER_SERVER_NAME must be a valid DNS name or IPv4 address" >&2
      exit 1
    fi
  done
else
  echo "SECONDBOX_RUNNER_SERVER_NAME must be a valid DNS name or IPv4 address" >&2
  exit 1
fi
if ! openssl x509 \
  -in "$runner_pki_directory/server.crt" \
  -noout \
  "$runner_server_check_option" "$runner_server_name" >/dev/null; then
  echo "Runner server certificate does not cover SECONDBOX_RUNNER_SERVER_NAME" >&2
  exit 1
fi
if [[ "$(value_for SECONDBOX_LISTEN_ADDR)" != "0.0.0.0:8080" ]]; then
  echo "SECONDBOX_LISTEN_ADDR must be 0.0.0.0:8080 for the packaged container mapping" >&2
  exit 1
fi
if [[ "$(value_for SECONDBOX_RUNNER_LISTEN_ADDR)" != "0.0.0.0:9443" ]]; then
  echo "SECONDBOX_RUNNER_LISTEN_ADDR must be 0.0.0.0:9443 for the packaged container mapping" >&2
  exit 1
fi

deployment_mode="$(value_for SECONDBOX_DEPLOYMENT_MODE)"
case "$deployment_mode" in
  development)
    if [[ "$(value_for SECONDBOX_API_BIND_IP)" != "127.0.0.1" ||
          "$(value_for SECONDBOX_RUNNER_BIND_IP)" != "127.0.0.1" ||
          "$(value_for SECONDBOX_POSTGRES_BIND_IP)" != "127.0.0.1" ||
          "$(value_for SECONDBOX_OBJECT_STORE_BIND_IP)" != "127.0.0.1" ]]; then
      echo "Development deployments must bind every published port to 127.0.0.1" >&2
      exit 1
    fi
    ;;
  production)
    if grep -Fq '"signatureKeyId": "secondbox-development-local-trust"' \
      "$signed_asset_catalog_host_path"; then
      echo "Production requires an operator-supplied signed asset catalog" >&2
      exit 1
    fi
    for image_setting in \
      SECONDBOX_CONTROL_PLANE_IMAGE \
      SECONDBOX_RUNNER_IMAGE \
      SECONDBOX_POSTGRES_IMAGE \
      SECONDBOX_OBJECT_STORE_IMAGE; do
      if [[ "$(value_for "$image_setting")" != *@sha256:* ]]; then
        echo "Production $image_setting must be pinned by digest" >&2
        exit 1
      fi
    done
    if [[ "$(value_for SECONDBOX_PUBLIC_BASE_URL)" != https://* ]]; then
      echo "Production SECONDBOX_PUBLIC_BASE_URL must use HTTPS" >&2
      exit 1
    fi
    if [[ "$(value_for SECONDBOX_OBJECT_STORE_ENDPOINT)" != https://* ]]; then
      echo "Production SECONDBOX_OBJECT_STORE_ENDPOINT must use HTTPS" >&2
      exit 1
    fi
    if [[ "$(value_for SECONDBOX_DATABASE_URL)" != *sslmode=verify-full* ]]; then
      echo "Production SECONDBOX_DATABASE_URL must use sslmode=verify-full" >&2
      exit 1
    fi
    if [[ "$(value_for SECONDBOX_TLS_TERMINATION)" != "external" ]]; then
      echo "Production SECONDBOX_TLS_TERMINATION must be external" >&2
      exit 1
    fi
    ;;
  *)
    echo "SECONDBOX_DEPLOYMENT_MODE must be development or production" >&2
    exit 1
    ;;
esac

if grep -Eq '^[A-Za-z_][A-Za-z0-9_]*=$' "$environment_path"; then
  echo "SecondBox environment contains a blank value" >&2
  exit 1
fi

echo "SecondBox environment validation passed: $environment_path"
