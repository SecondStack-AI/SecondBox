#!/usr/bin/env bash
set -Eeuo pipefail

report_qualified_guest_failure() {
  local status="$?" line="$1" log
  trap - ERR
  echo "qualified guest failed: phase=${phase:-setup} mode=${mode:-unknown} line=$line status=$status" >&2
  if [[ -d "${qualification_root:-}" ]]; then
    while IFS= read -r -d '' log; do
      echo "qualified guest log tail: $(basename "$log")" >&2
      tail -n 100 -- "$log" >&2
    done < <(find "$qualification_root" -maxdepth 1 -type f -name '*.log' -print0 | sort -z)
  fi
  return "$status"
}

trap 'report_qualified_guest_failure "$LINENO"' ERR

phase="${1:-}"
mode="${2:-}"
release_directory="${3:-}"
v060_adapter="${4:-}"
[[ "$phase" == install || "$phase" == verify ]] || { echo 'qualified guest phase must be install or verify' >&2; exit 2; }
[[ "$mode" == btrfs_image || "$mode" == existing_reflink_filesystem || "$mode" == existing_reflink_update ]] || { echo 'qualified guest mode is invalid' >&2; exit 2; }
[[ "$release_directory" == /* && -d "$release_directory" && ! -L "$release_directory" ]] || { echo 'qualified guest release directory is unsafe' >&2; exit 1; }
if [[ "$mode" == existing_reflink_update ]]; then
  [[ "$v060_adapter" == /* && -f "$v060_adapter" && ! -L "$v060_adapter" && -x "$v060_adapter" ]] || { echo 'qualified guest requires the v0.6.0 recorded-waiver adapter' >&2; exit 1; }
  jq -e '.version == "0.6.0" and .sourceCommit == "92e409ddade89737afa75ec2b781dac5c8afbeab"' <("$v060_adapter" --output json version) >/dev/null
fi

qualification_root="$HOME/.secondbox-installer-qualification"
state="$qualification_root/state-${mode}.json"
evidence="/opt/secondbox-qualification/evidence-${mode}.json"
mkdir -p "$qualification_root"
mapfile -t manifests < <(find "$release_directory" -maxdepth 1 -type f -name 'secondbox-*-artifact-manifest.json' -print)
[[ "${#manifests[@]}" -eq 1 ]] || { echo 'qualified guest requires exactly one artifact manifest' >&2; exit 1; }
manifest="${manifests[0]}"
deploy_name="$(jq -er '.binaries[] | select(.name == "secondbox-deploy" and .platform == "linux/amd64") | .location | split("/")[-1]' "$manifest")"
deploy="$release_directory/$deploy_name"
[[ -f "$deploy" && ! -L "$deploy" ]] || { echo 'qualified guest deployment binary is absent' >&2; exit 1; }
chmod 0755 "$deploy"

artifact_fingerprint() {
  local directory="$1"
  find "$directory" -xdev -type f -printf '%P\0%s\0%i\0%C@\0' | sort -z | sha256sum | awk '{print $1}'
}

assertions_json() {
  local ids=() id
  for id in "$@"; do ids+=("$(jq -nc --arg id "$id" '{id:$id,passed:true}')"); done
  printf '%s\n' "${ids[@]}" | jq -s .
}

setup_candidate_registry() {
  local registry_image='docker.io/library/registry@sha256:46faa9a1ae6813194b53921a370f2f4f8c5e1aae228a89bceafef5847a6a3278' certificate_dir="$qualification_root/registry-certs"
  local skopeo_home="$qualification_root/skopeo-home"
  local name archive reference repository actual

  docker pull "$registry_image" >/dev/null
  for reference in \
    "$(jq -er .bundledServices.postgres "$manifest")"; do
    docker pull "$reference" >/dev/null
  done

  mkdir -p "$certificate_dir"
  install -d -m 0700 "$skopeo_home"
  openssl req -x509 -newkey rsa:3072 -sha256 -days 2 -nodes -subj '/CN=ghcr.io' \
    -addext 'subjectAltName=DNS:ghcr.io' -keyout "$certificate_dir/tls.key" -out "$certificate_dir/tls.crt" >/dev/null 2>&1
  sudo install -m 0644 "$certificate_dir/tls.crt" /usr/local/share/ca-certificates/secondbox-qualification-ghcr.crt
  sudo install -d -m 0755 /etc/docker/certs.d/ghcr.io
  sudo install -m 0644 "$certificate_dir/tls.crt" /etc/docker/certs.d/ghcr.io/ca.crt
  sudo update-ca-certificates >/dev/null
  grep -qE '^[[:space:]]*127\.0\.0\.1[[:space:]]+ghcr\.io([[:space:]]|$)' /etc/hosts || printf '127.0.0.1 ghcr.io\n' | sudo tee -a /etc/hosts >/dev/null
  docker rm -f secondbox-qualification-registry >/dev/null 2>&1 || true
  docker run -d --name secondbox-qualification-registry --restart unless-stopped -p 443:5000 \
    -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/tls.crt -e REGISTRY_HTTP_TLS_KEY=/certs/tls.key \
    -v "$certificate_dir:/certs:ro" "$registry_image" >/dev/null
  for _ in $(seq 1 60); do curl --fail --silent https://ghcr.io/v2/ >/dev/null 2>&1 && break; sleep 1; done
  curl --fail --silent https://ghcr.io/v2/ >/dev/null

  while read -r name archive reference; do
    [[ -f "$release_directory/$archive" && ! -L "$release_directory/$archive" ]] || { echo "qualified guest OCI archive is absent: $archive" >&2; exit 1; }
    repository="${reference%@*}"
    HOME="$skopeo_home" skopeo copy --all "oci-archive:$release_directory/$archive" "docker://$repository:qualification-$mode" >/dev/null
    actual="sha256:$(HOME="$skopeo_home" skopeo inspect --raw "docker://$reference" | sha256sum | awk '{print $1}')"
    [[ "$actual" == "${reference#*@}" ]] || { echo "qualified guest $name registry digest mismatch: $actual" >&2; exit 1; }
    docker pull "$reference" >/dev/null
    docker image inspect "$reference" >/dev/null
  done <<EOF
control-plane control-plane.oci.tar $(jq -er .controlPlane.reference "$manifest")
runner runner.oci.tar $(jq -er .runner.reference "$manifest")
installer-tools installer-tools.oci.tar $(jq -er .installerTools.reference "$manifest")
microvm-artifacts microvm-artifacts.oci.tar $(jq -er .microvm.imageReference "$manifest")
EOF
}

create_qualification_workload() {
  local plan_path="$1" binary="$2" root="$3" tenant_ref="$4" key_suffix="$5"
  local platform_config="$root/platform.json" controller_config="$root/controller.json" application_config="$root/application.json"
  local platform_token_path platform_token api_address subject_ref expiry controller_response controller_token application_response application_token
  local sandbox_operation sandbox_id sandbox_state=''

  install -d -m 0700 "$root"
  platform_token_path="$(jq -er '.secretTargets[] | select(.category == "platform-authority") | .path' "$plan_path")"
  platform_token="$(tr -d '\n' <"$platform_token_path")"
  api_address="$(jq -er .network.apiAddress "$plan_path")"
  subject_ref='qualified-subject'
  expiry="$(date -u -d '+6 hours' '+%Y-%m-%dT%H:%M:%SZ')"

  SECONDBOX_CONFIG="$platform_config" SECONDBOX_URL="http://$api_address" SECONDBOX_TOKEN="$platform_token" \
    "$binary" --output json platform login >/dev/null
  jq -n --arg ref "$tenant_ref" \
    '{ref:$ref,allowedProfileGrants:["durable-coding"],allowedApplicationScopes:["sandbox:read","sandbox:lifecycle","sandbox:exec","sandbox:files","sandbox:ports"],aggregateQuota:{maxSandboxes:1,maxActiveInstances:1,maxVcpuCount:4,maxMemoryBytes:8589934592,maxSnapshots:1,maxPortSessions:1,maxConcurrentOperations:16,maxActiveSubjects:1,maxApplicationAuthorities:1},expiryPolicy:{maximumSubjectLifetimeSeconds:21600,maximumAuthorityLifetimeSeconds:21600},metadata:{qualification:"installer-qualified-workload"}}' >"$root/tenant.json"
  SECONDBOX_CONFIG="$platform_config" "$binary" --output json tenant create \
    --file "$root/tenant.json" --idempotency-key "qualified-tenant-$key_suffix" >/dev/null
  jq -n --arg expiresAt "$expiry" '{expiresAt:$expiresAt,metadata:{qualification:"installer-qualified-workload"}}' >"$root/controller-request.json"
  controller_response="$(SECONDBOX_CONFIG="$platform_config" "$binary" --output json tenant controller-authority create \
    "$tenant_ref" --file "$root/controller-request.json" --idempotency-key "qualified-controller-$key_suffix")"
  controller_token="$(jq -er .bearerToken <<<"$controller_response")"
  SECONDBOX_CONFIG="$controller_config" SECONDBOX_URL="http://$api_address" SECONDBOX_TOKEN="$controller_token" \
    "$binary" --output json controller login >/dev/null
  jq -n --arg ref "$subject_ref" \
    '{ref:$ref,quota:{maxSandboxes:1,maxActiveInstances:1,maxVcpuCount:4,maxMemoryBytes:8589934592,maxSnapshots:1,maxPortSessions:1,maxConcurrentOperations:16},metadata:{qualification:"installer-qualified-workload"}}' >"$root/subject.json"
  SECONDBOX_CONFIG="$controller_config" "$binary" --output json subject create \
    --file "$root/subject.json" --idempotency-key "qualified-subject-$key_suffix" >/dev/null
  jq -n --arg subjectRef "$subject_ref" --arg expiresAt "$expiry" \
    '{subjectRef:$subjectRef,scopes:["sandbox:read","sandbox:lifecycle","sandbox:exec","sandbox:files","sandbox:ports"],profileGrants:["durable-coding"],expiresAt:$expiresAt,metadata:{qualification:"installer-qualified-workload"}}' >"$root/application-request.json"
  application_response="$(SECONDBOX_CONFIG="$controller_config" "$binary" --output json application-authority create \
    --file "$root/application-request.json" --idempotency-key "qualified-application-$key_suffix")"
  application_token="$(jq -er .bearerToken <<<"$application_response")"
  SECONDBOX_CONFIG="$application_config" SECONDBOX_URL="http://$api_address" SECONDBOX_TOKEN="$application_token" \
    SECONDBOX_TENANT_REF="$tenant_ref" SECONDBOX_SUBJECT_REF="$subject_ref" \
    "$binary" --output json application login >/dev/null
  jq -n '{profile:"durable-coding",metadata:{qualification:"installer-qualified-workload"}}' >"$root/sandbox.json"
  sandbox_operation="$(SECONDBOX_CONFIG="$application_config" "$binary" --output json sandboxes create \
    --body "$root/sandbox.json" --header "Idempotency-Key=qualified-sandbox-$key_suffix")"
  sandbox_id="$(jq -er .sandboxId <<<"$sandbox_operation")"
  for _ in $(seq 1 300); do
    sandbox_state="$(SECONDBOX_CONFIG="$application_config" "$binary" --output json sandboxes get --path "sandboxId=$sandbox_id" | jq -er .state)"
    [[ "$sandbox_state" == ready ]] && break
    [[ "$sandbox_state" != failed ]] || { echo 'explicit qualification Sandbox failed while starting' >&2; return 1; }
    sleep 1
  done
  [[ "$sandbox_state" == ready ]] || { echo 'explicit qualification Sandbox did not become ready' >&2; return 1; }
  jq -nc --arg sandboxId "$sandbox_id" --arg configPath "$application_config" '{sandboxId:$sandboxId,configPath:$configPath}'
}

if [[ "$phase" == install ]]; then
  [[ ! -e "$HOME/.config/secondbox/config.json" ]] || { echo 'qualified guest is not a clean CLI host' >&2; exit 1; }
  [[ -z "$(find "$HOME" -maxdepth 1 -type d -name 'secondbox-install_*' -print -quit)" ]] || { echo 'qualified guest has a prior installer operation' >&2; exit 1; }
  [[ -z "$(docker ps -a --filter label=com.docker.compose.project --format '{{.Names}}' | grep secondbox || true)" ]] || { echo 'qualified guest has prior SecondBox containers' >&2; exit 1; }

  if [[ "$mode" == existing_reflink_filesystem || "$mode" == existing_reflink_update ]]; then
    sudo mkfs.btrfs -q -f -L secondbox-qualification /dev/vdb
    sudo install -d -m 0755 /srv/secondbox-dedicated
    data_uuid="$(sudo blkid -s UUID -o value /dev/vdb)"
    printf 'UUID=%s /srv/secondbox-dedicated btrfs defaults,nofail 0 2\n' "$data_uuid" | sudo tee -a /etc/fstab >/dev/null
    sudo mount /srv/secondbox-dedicated
    sudo chown "$USER:$USER" /srv/secondbox-dedicated
  fi

  before_mounts="$(findmnt --json --output TARGET,SOURCE,FSTYPE | sha256sum | awk '{print $1}')"
  "$deploy" --output json install --check >"$qualification_root/preflight-${mode}.json"
  after_mounts="$(findmnt --json --output TARGET,SOURCE,FSTYPE | sha256sum | awk '{print $1}')"
  [[ "$before_mounts" == "$after_mounts" ]] || { echo 'installer preflight changed the mount table' >&2; exit 1; }
  [[ -z "$(find "$HOME" -maxdepth 1 -type d -name 'secondbox-install_*' -print -quit)" ]] || { echo 'installer preflight created an operation directory' >&2; exit 1; }

  bootstrap="$(jq -er '.installBootstrap.location | split("/")[-1]' "$manifest")"
  bootstrap_digest="$(jq -er '.installBootstrap.digest | sub("^sha256:"; "")' "$manifest")"
  printf '%s  %s\n' "$bootstrap_digest" "$release_directory/$bootstrap" | sha256sum --check --status
  binary_digest="$(jq -er '.binaries[] | select(.name == "secondbox-deploy" and .platform == "linux/amd64") | .sha256' "$manifest")"
  printf '%s  %s\n' "$binary_digest" "$deploy" | sha256sum --check --status
  grep -F -- "$binary_digest" "$release_directory/$bootstrap" >/dev/null

  update_attempt='not_run'
  source_sandbox_lineage=''
  qualification_sandbox_id=''
  qualification_workload_config=''
  source_deploy=''
  if [[ "$mode" == existing_reflink_update ]]; then
    source_version='0.6.0'
    source_bootstrap_sha256='83f64289be964206563bebcc96796f081fa2d1b1d84915a5f6722ef1962f6593'
    source_binary_digest='947d8f600d2fcd88c0d732f3b0b64839d7409e50ebd01655cb5bb9a9789aceeb'
    source_bootstrap="$qualification_root/source-install.sh"
    curl --fail --location --proto '=https' --tlsv1.2 --output "$source_bootstrap" "https://github.com/SecondStack-AI/SecondBox/releases/download/v${source_version}/install.sh"
    printf '%s  %s\n' "$source_bootstrap_sha256" "$source_bootstrap" | sha256sum --check --status
    bootstrap_version="$(sed -n "s/^version='\([^']*\)'$/\1/p" "$source_bootstrap")"
    bootstrap_binary_digest="$(sed -n "s/^expected_sha256='\([0-9a-f]\{64\}\)'$/\1/p" "$source_bootstrap")"
    candidate_version="$(jq -er .version "$manifest")"
    [[ "$bootstrap_version" == "$source_version" && "$bootstrap_binary_digest" == "$source_binary_digest" && "$source_version" != "$candidate_version" ]] || { echo 'qualified update source does not match the pinned v0.6.0 release' >&2; exit 1; }
    source_deploy="$qualification_root/secondbox-deploy-source"
    curl --fail --location --proto '=https' --tlsv1.2 --output "$source_deploy" "https://github.com/SecondStack-AI/SecondBox/releases/download/v${source_version}/secondbox-deploy_${source_version}_linux_amd64"
    printf '%s  %s\n' "$source_binary_digest" "$source_deploy" | sha256sum --check --status
    chmod 0755 "$source_deploy"
    install_log="$qualification_root/install-source-${mode}.log"
    # The exact public binary is downloaded and digest-checked above. The
    # qualification-only adapter is the same tagged source with only the
    # published waiver recognition patch, allowing it to install the otherwise
    # immutable public v0.6.0 release and establish a real update source.
    printf '1\ny\n1\ny\ny\n' | "$v060_adapter" --accessible install >"$install_log" 2>&1
    operation="$(find "$HOME" -maxdepth 1 -type d -name 'secondbox-install_*' -print -quit)"
    [[ -n "$operation" && -f "$operation/install-plan.json" && -f "$operation/install-receipt.json" ]] || { echo 'qualified source installation is absent' >&2; exit 1; }
    plan="$operation/install-plan.json"
    receipt="$operation/install-receipt.json"
    jq -e --arg source "$source_version" '(.schemaVersion == "secondbox.install.plan/v1" or .schemaVersion == "secondbox.install.plan/v2") and .release.version == $source' "$plan" >/dev/null
    jq -e '.status == "succeeded" and (.schemaVersion == "secondbox.install.receipt/v1" or .schemaVersion == "secondbox.install.receipt/v2")' "$receipt" >/dev/null
    cli_binary="$(jq -er '.paths[] | select(.name == "secondbox-binary") | .path' "$plan")"
    cli_config="$(jq -er .cli.configPath "$plan")"
    source_qualification="$(jq -r '.completedStages[] | select(.stage == "smoke_execution") | .evidence.qualification // ""' "$receipt")"
    if [[ "$source_qualification" == authenticated-runner-readiness ]]; then
      source_workload="$(create_qualification_workload "$plan" "$cli_binary" "$qualification_root/source-workload-$mode" "qualified-$mode" "source-$mode")"
      qualification_sandbox_id="$(jq -er .sandboxId <<<"$source_workload")"
      qualification_workload_config="$(jq -er .configPath <<<"$source_workload")"
    else
      qualification_workload_config="$cli_config"
      qualification_sandbox_id="$(SECONDBOX_CONFIG="$qualification_workload_config" "$cli_binary" --output json sandboxes list --query limit=1 | jq -er '.items[0].id')"
    fi
    source_sandbox_lineage="$(SECONDBOX_CONFIG="$qualification_workload_config" "$cli_binary" --output json sandboxes get --path "sandboxId=$qualification_sandbox_id" | jq -cS '{id,profile,profileRevisionId,workspaceId:.workspace.id}')"
    SECONDBOX_CONFIG="$qualification_workload_config" "$cli_binary" --output plain exec "$qualification_sandbox_id" -- python3 -c 'open("/workspace/update-preserved.txt","w").write("preserved through update\n")'
    source_sandbox_revision="$(SECONDBOX_CONFIG="$qualification_workload_config" "$cli_binary" --output json sandboxes get --path "sandboxId=$qualification_sandbox_id" | jq -er .revision)"
    SECONDBOX_CONFIG="$qualification_workload_config" "$cli_binary" --output json sandboxes stop \
      --path "sandboxId=$qualification_sandbox_id" \
      --header "If-Match=\"revision-${source_sandbox_revision}\"" \
      --header "Idempotency-Key=qualification-update-stop-${qualification_sandbox_id}" >/dev/null
    for _ in $(seq 1 300); do
      source_state="$(SECONDBOX_CONFIG="$qualification_workload_config" "$cli_binary" --output json sandboxes get --path "sandboxId=$qualification_sandbox_id" | jq -er .state)"
      [[ "$source_state" == stopped ]] && break
      [[ "$source_state" != failed ]] || { echo 'qualified update source Sandbox failed while stopping' >&2; exit 1; }
      sleep 1
    done
    [[ "$source_state" == stopped ]] || { echo 'qualified update source Sandbox did not stop' >&2; exit 1; }
    setup_candidate_registry
    if ! "$deploy" --output json update --check "$operation" --candidate-directory "$release_directory" >"$qualification_root/update-check-${mode}.json" 2>"$qualification_root/update-check-${mode}.log"; then
      echo 'qualified v0.6.0 update rejected the candidate release' >&2
      tail -n 100 "$qualification_root/update-check-${mode}.log" >&2
      exit 1
    fi
    printf 'y\n' | "$deploy" --accessible update "$operation" --candidate-directory "$release_directory" >"$qualification_root/update-${mode}.log" 2>&1
    jq -e --arg source "$source_version" --arg target "$candidate_version" '
      .schemaVersion == "secondbox.install.receipt/v2" and
      .updates[-1].status == "succeeded" and
      .updates[-1].sourceRelease.version == $source and
      .updates[-1].targetRelease.version == $target and
      .updates[-1].completedStages[-1].stage == "smoke_execution" and
      .updates[-1].completedStages[-1].evidence.qualification == "authenticated-runner-readiness" and
      .updates[-1].completedStages[-1].evidence.runnerPool == "standard-amd64" and
      .updates[-1].completedStages[-1].evidence.runnerState == "ready" and
      .updates[-1].completedStages[-1].evidence.runnerCredentialState == "pre_shared" and
      .updates[-1].completedStages[-1].evidence.runnerPoolState == "ready" and
      (.updates[-1].completedStages[-1].evidence.runnerPoolReadyRunners | tonumber) >= 1 and
      .updates[-1].completedStages[-1].evidence.coldBootCapacity == "advertised" and
      (.updates[-1].completedStages[-1].evidence.concurrentOperationCapacity | tonumber) >= 16 and
      .updates[-1].completedStages[-1].evidence.postUpdateState == "runner-ready"
    ' "$receipt" >/dev/null
    jq -e --arg source "$source_version" --arg target "$candidate_version" '
      .schemaVersion == "secondbox.install.plan/v2" and .release.version == $target and
      .releaseHistory[0].release.version == $source and .releaseHistory[-1].release.version == $target
    ' "$plan" >/dev/null
    update_attempt='succeeded'
  else
    setup_candidate_registry
    install_log="$qualification_root/install-${mode}.log"
    setsid bash -c 'printf "1\ny\n1\ny\ny\n" | "$1" --accessible install --candidate-directory "$2"' bash "$deploy" "$release_directory" >"$install_log" 2>&1 &
    install_pid=$!
    operation=''
    interrupted=false
    for _ in $(seq 1 3600); do
      operation="$(find "$HOME" -maxdepth 1 -type d -name 'secondbox-install_*' -print -quit)"
      if [[ -n "$operation" && -f "$operation/install-receipt.json" ]] && jq -e '[.completedStages[].stage] | index("assets_materialized") != null and index("deployment_materialized") == null' "$operation/install-receipt.json" >/dev/null 2>&1; then
        kill -TERM -- "-$install_pid" >/dev/null 2>&1 || true
        wait "$install_pid" >/dev/null 2>&1 || true
        interrupted=true
        break
      fi
      if ! kill -0 "$install_pid" >/dev/null 2>&1; then break; fi
      sleep 0.1
    done
    $interrupted || { echo 'qualified guest could not interrupt the installer after verified materialization' >&2; tail -n 100 "$install_log" >&2; exit 1; }
  fi
  [[ -n "$operation" && -f "$operation/install-plan.json" && -f "$operation/install-receipt.json" ]] || { echo 'qualified guest interrupted operation is absent' >&2; exit 1; }
  plan="$operation/install-plan.json"
  receipt="$operation/install-receipt.json"
  workspace="$(jq -er .storage.workspacePath "$plan")"
  artifacts="$(jq -er '.paths[] | select(.name == "artifacts") | .path' "$plan")"
  manifest_path="$(jq -er '.paths[] | select(.name == "manifest") | .path' "$plan")"
  cli_binary="$(jq -er '.paths[] | select(.name == "secondbox-binary") | .path' "$plan")"
  cli_config="$(jq -er .cli.configPath "$plan")"
  operation_id="$(jq -er .operationId "$plan")"
  filesystem_identity="$(findmnt -n -o MAJ:MIN --target "$workspace" 2>/dev/null || stat -c '%d' "$workspace")"
  neighbor="$HOME/secondbox-qualification-neighbor-$mode"
  printf 'preserve me\n' >"$neighbor"
  jq -n --arg operation "$operation" --arg operationId "$operation_id" --arg workspace "$workspace" \
    --arg artifacts "$artifacts" --arg manifest "$manifest_path" --arg cliBinary "$cli_binary" --arg cliConfig "$cli_config" \
    --arg artifactFingerprint "$(artifact_fingerprint "$artifacts")" --arg filesystemIdentity "$filesystem_identity" --arg neighbor "$neighbor" \
    --arg sandboxId "$qualification_sandbox_id" --arg workloadConfig "$qualification_workload_config" \
    --arg sourceSandboxLineage "$source_sandbox_lineage" --arg updateAttempt "$update_attempt" \
    '{operation:$operation,operationId:$operationId,workspace:$workspace,artifacts:$artifacts,manifest:$manifest,cliBinary:$cliBinary,cliConfig:$cliConfig,artifactFingerprint:$artifactFingerprint,filesystemIdentity:$filesystemIdentity,neighbor:$neighbor,sandboxId:$sandboxId,workloadConfig:$workloadConfig,updateAttempt:$updateAttempt,sourceSandboxLineage:$sourceSandboxLineage}' >"$state"
  exit 0
fi

[[ -f "$state" && ! -L "$state" ]] || { echo 'qualified guest state is absent after reboot' >&2; exit 1; }
operation="$(jq -er .operation "$state")"
operation_id="$(jq -er .operationId "$state")"
workspace="$(jq -er .workspace "$state")"
artifacts="$(jq -er .artifacts "$state")"
manifest_path="$(jq -er .manifest "$state")"
cli_binary="$(jq -er .cliBinary "$state")"
cli_config="$(jq -er .cliConfig "$state")"
neighbor="$(jq -er .neighbor "$state")"
receipt="$operation/install-receipt.json"
plan="$operation/install-plan.json"
[[ -c /dev/kvm && -r /dev/kvm && -w /dev/kvm && -c /dev/net/tun ]] || { echo 'nested virtualization devices did not survive reboot' >&2; exit 1; }
findmnt --target "$workspace" --types btrfs >/dev/null

if [[ "$mode" == existing_reflink_filesystem || "$mode" == existing_reflink_update ]]; then
  isolation="$workspace/.qualification-reflink"
  sudo install -d -m 0700 -o root -g root -- "$isolation"
  printf 'source\n' | sudo tee -- "$isolation/source" >/dev/null
  sudo cp --reflink=always -- "$isolation/source" "$isolation/copy"
  printf 'copy changed\n' | sudo tee -- "$isolation/copy" >/dev/null
  [[ "$(sudo cat -- "$isolation/source")" == source ]]
  jq -e '.storage.choice == "existing_mount" and (.storage.existingDeviceIdentity | length > 0) and ([.paths[].path | select(startswith("/dev"))] | length == 0)' "$plan" >/dev/null
  if [[ "$mode" == existing_reflink_filesystem ]]; then
    sudo mount -t tmpfs -o size=1m,nosuid,nodev,noexec tmpfs /srv/secondbox-dedicated
    if "$deploy" --accessible install --resume "$operation" --candidate-directory "$release_directory" >"$qualification_root/unsafe-filesystem-${mode}.log" 2>&1; then
      echo 'resume accepted a replacement filesystem at the reviewed existing mount' >&2
      exit 1
    fi
    sudo umount /srv/secondbox-dedicated
  fi
fi

if [[ "$mode" != existing_reflink_update ]]; then
  "$deploy" --accessible install --resume "$operation" --candidate-directory "$release_directory" >"$qualification_root/resume-${mode}.log" 2>&1
fi
jq -e '.status == "succeeded" and .completedStages[-1].stage == "smoke_execution"' "$receipt" >/dev/null
[[ "$(artifact_fingerprint "$artifacts")" == "$(jq -er .artifactFingerprint "$state")" ]] || { echo 'verified artifact bundle was unexpectedly re-extracted' >&2; exit 1; }
compose_project="$(jq -er '.completedStages[] | select(.stage == "compose_started") | .evidence.composeProject' "$receipt")"
mapfile -t running_services < <(
  docker ps --filter "label=com.docker.compose.project=$compose_project" \
    --format '{{.Label "com.docker.compose.service"}}' | sort
)
[[ "${running_services[*]}" == 'control-plane postgres same-host-runner' ]]
jq -e '.completedStages[] | select(.stage == "readiness") | .evidence.runnerState == "ready"' "$receipt" >/dev/null
jq -e '.completedStages[] | select(.stage == "cli_login")' "$receipt" >/dev/null
expected_runner_id="runner-${operation_id#install_}"
update_attempt="$(jq -er .updateAttempt "$state")"
jq -e --arg runnerId "$expected_runner_id" '
  .completedStages[] | select(.stage == "smoke_execution") |
  .evidence.qualification == "authenticated-runner-readiness" and
  .evidence.runnerId == $runnerId and
  .evidence.runnerPool == "standard-amd64" and
  .evidence.runnerState == "ready" and
  .evidence.runnerCredentialState == "pre_shared" and
  .evidence.runnerPoolState == "ready" and
  (.evidence.runnerPoolReadyRunners | tonumber) >= 1 and
  .evidence.coldBootCapacity == "advertised" and
  (.evidence.concurrentOperationCapacity | tonumber) >= 16
' "$receipt" >/dev/null
if [[ "$mode" == existing_reflink_update ]]; then
  sandbox_id="$(jq -er '.sandboxId | select(length > 0)' "$state")"
  workload_config="$(jq -er '.workloadConfig | select(length > 0)' "$state")"
else
  workload="$(create_qualification_workload "$plan" "$cli_binary" "$qualification_root/clean-install-$mode" "qualified-$mode" "$mode")"
  sandbox_id="$(jq -er .sandboxId <<<"$workload")"
  workload_config="$(jq -er .configPath <<<"$workload")"
fi

live_runner_state=''
for _ in $(seq 1 300); do
  if live_runners="$(SECONDBOX_CONFIG="$cli_config" "$cli_binary" --output json runners list 2>/dev/null)"; then
    live_runner_state="$(jq -r --arg id "$expected_runner_id" '.items[] | select(.id == $id) | .state' <<<"$live_runners")"
  fi
  [[ "$live_runner_state" == ready ]] && break
  sleep 1
done
[[ "$live_runner_state" == ready ]] || { echo 'installed Runner did not become ready after reboot' >&2; exit 1; }
if [[ "$mode" == existing_reflink_update ]]; then
  sandbox_after_update_document="$(SECONDBOX_CONFIG="$workload_config" "$cli_binary" --output json sandboxes get --path "sandboxId=$sandbox_id")"
  [[ "$(jq -er .state <<<"$sandbox_after_update_document")" == stopped ]] || { echo 'retained qualification Sandbox was not stopped after release update and reboot' >&2; exit 1; }
  sandbox_after_update_revision="$(jq -er .revision <<<"$sandbox_after_update_document")"
  SECONDBOX_CONFIG="$workload_config" "$cli_binary" --output json sandboxes start \
    --path "sandboxId=$sandbox_id" \
    --header "If-Match=\"revision-${sandbox_after_update_revision}\"" \
    --header "Idempotency-Key=qualification-update-reboot-start-${sandbox_id}" >/dev/null
  for _ in $(seq 1 300); do
    sandbox_after_update_state="$(SECONDBOX_CONFIG="$workload_config" "$cli_binary" --output json sandboxes get --path "sandboxId=$sandbox_id" | jq -er .state)"
    [[ "$sandbox_after_update_state" == ready ]] && break
    [[ "$sandbox_after_update_state" != failed ]] || { echo 'retained qualification Sandbox failed while starting after release update and reboot' >&2; exit 1; }
    sleep 1
  done
  [[ "$sandbox_after_update_state" == ready ]] || { echo 'retained qualification Sandbox did not become ready after release update and reboot' >&2; exit 1; }
fi
SECONDBOX_CONFIG="$workload_config" "$cli_binary" --output plain exec "$sandbox_id" -- python3 -c 'print("hello after reboot")' | grep -Fx 'hello after reboot' >/dev/null
sandbox_before_document="$(SECONDBOX_CONFIG="$workload_config" "$cli_binary" --output json sandboxes get --path "sandboxId=$sandbox_id")"
sandbox_before="$(jq -cS '{id,profile,profileRevisionId,workspaceId:.workspace.id}' <<<"$sandbox_before_document")"
if [[ "$mode" == existing_reflink_update ]]; then
  [[ "$sandbox_before" == "$(jq -er .sourceSandboxLineage "$state")" ]] || { echo 'retained Sandbox lineage changed across release update' >&2; exit 1; }
  SECONDBOX_CONFIG="$workload_config" "$cli_binary" --output plain exec "$sandbox_id" -- python3 -c 'print(open("/workspace/update-preserved.txt").read(), end="")' | grep -Fx 'preserved through update' >/dev/null
fi
generation_before="$(jq -er '.generation | select(type == "number" and . >= 1)' <<<"$sandbox_before_document")"
workspace_generation_before="$(jq -er '.workspace.generation | select(type == "number" and . >= 1)' <<<"$sandbox_before_document")"
(( workspace_generation_before == generation_before )) || { printf 'retained qualification Sandbox and Workspace generations differ before uninstall: sandbox=%s workspace=%s\n' "$generation_before" "$workspace_generation_before" >&2; exit 1; }

lifecycle_deploy="$deploy"
"$lifecycle_deploy" --accessible uninstall "$operation" >"$qualification_root/uninstall-${mode}.log" 2>&1
jq -e '.status == "uninstalled"' "$receipt" >/dev/null
[[ -d "$workspace" && -d "$artifacts" && -f "$manifest_path" ]]
"$lifecycle_deploy" --accessible install --resume "$operation" --candidate-directory "$release_directory" >"$qualification_root/resume-after-uninstall-${mode}.log" 2>&1
sandbox_after_document="$(SECONDBOX_CONFIG="$workload_config" "$cli_binary" --output json sandboxes get --path "sandboxId=$sandbox_id")"
sandbox_after="$(jq -cS '{id,profile,profileRevisionId,workspaceId:.workspace.id}' <<<"$sandbox_after_document")"
generation_after="$(jq -er '.generation | select(type == "number" and . >= 1)' <<<"$sandbox_after_document")"
workspace_generation_after="$(jq -er '.workspace.generation | select(type == "number" and . >= 1)' <<<"$sandbox_after_document")"
[[ "$sandbox_before" == "$sandbox_after" ]] || { printf 'retained qualification Sandbox lineage changed across uninstall/resume: before=%s after=%s\n' "$sandbox_before" "$sandbox_after" >&2; exit 1; }
(( generation_after >= generation_before )) || { printf 'retained qualification Sandbox generation regressed across uninstall/resume: before=%s after=%s\n' "$generation_before" "$generation_after" >&2; exit 1; }
(( workspace_generation_after >= workspace_generation_before )) || { printf 'retained qualification Workspace generation regressed across uninstall/resume: before=%s after=%s\n' "$workspace_generation_before" "$workspace_generation_after" >&2; exit 1; }
(( workspace_generation_after == generation_after )) || { printf 'retained qualification Sandbox and Workspace generations differ after resume: sandbox=%s workspace=%s\n' "$generation_after" "$workspace_generation_after" >&2; exit 1; }
"$lifecycle_deploy" --accessible uninstall "$operation" >/dev/null 2>&1

sudo install -d -m 0755 "$workspace/.qualification-foreign-mount"
sudo mount -t tmpfs -o size=1m,nosuid,nodev,noexec tmpfs "$workspace/.qualification-foreign-mount"
if printf 'PURGE %s\n' "$operation_id" | "$lifecycle_deploy" --accessible uninstall --purge "$operation" >"$qualification_root/foreign-purge-${mode}.log" 2>&1; then
  echo 'purge accepted an unrecorded nested mount' >&2
  exit 1
fi
sudo mountpoint -q "$workspace/.qualification-foreign-mount"
sudo umount "$workspace/.qualification-foreign-mount"
sudo rmdir "$workspace/.qualification-foreign-mount"
mapfile -t purge_paths < <(jq -er '.createdResources[] | select(.id != "operation-directory" and (.path | type) == "string" and (.path | length) > 0) | .path' "$receipt")
printf 'PURGE %s\n' "$operation_id" | "$lifecycle_deploy" --accessible uninstall --purge "$operation" >"$qualification_root/purge-${mode}.log" 2>&1
jq -e '.status == "purged"' "$receipt" >/dev/null
[[ -f "$neighbor" && "$(<"$neighbor")" == 'preserve me' ]]
for purge_path in "${purge_paths[@]}"; do
  [[ ! -e "$purge_path" ]] || { echo "purge retained a receipt-owned path: $purge_path" >&2; exit 1; }
done
[[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$compose_project")" ]]
[[ -z "$(docker volume ls -q --filter "label=com.docker.compose.project=$compose_project")" ]]
[[ -z "$(docker network ls -q --filter "label=com.docker.compose.project=$compose_project")" ]]
[[ -f "$operation/install-plan.json" && -f "$receipt" ]]

common=(clean_host read_only_preflight bootstrap_checksum guided_install reboot_recovery mount_recovery compose_ready runner_ready cli_login clean_install_delegated_workflow hello_microvm stage_interrupt_resume verified_bundle_not_reextracted uninstall_workspace_preserved resume_same_sandbox_lineage purge_exact_resources purge_neighbor_preserved purge_foreign_mount_refused)
if [[ "$mode" == existing_reflink_filesystem ]]; then
  common+=(existing_reflink_isolation unsafe_filesystem_refusals fresh_existing_reflink_install)
elif [[ "$mode" == existing_reflink_update ]]; then
  common=(update_compatibility_enforced update_receipt_history_preserved update_release_activated update_workspace_preserved)
fi
jq -n --arg mode "$mode" --arg filesystemIdentity "$(jq -er .filesystemIdentity "$state")" \
  --arg candidateManifestDigest "sha256:$(sha256sum "$manifest" | awk '{print $1}')" --argjson assertions "$(assertions_json "${common[@]}")" \
  '{mode:$mode,passed:true,rebootPassed:true,candidateManifestDigest:$candidateManifestDigest,filesystemIdentity:$filesystemIdentity,assertions:$assertions}' >"$evidence"
chmod 0644 "$evidence"
