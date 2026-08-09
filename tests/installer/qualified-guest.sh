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
[[ "$phase" == install || "$phase" == verify ]] || { echo 'qualified guest phase must be install or verify' >&2; exit 2; }
[[ "$mode" == btrfs_image || "$mode" == existing_reflink_filesystem ]] || { echo 'qualified guest mode is invalid' >&2; exit 2; }
[[ "$release_directory" == /* && -d "$release_directory" && ! -L "$release_directory" ]] || { echo 'qualified guest release directory is unsafe' >&2; exit 1; }

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
  local name archive reference repository actual

  docker pull "$registry_image" >/dev/null
  for reference in \
    "$(jq -er .bundledServices.postgres "$manifest")" \
    "$(jq -er .bundledServices.objectStore "$manifest")" \
    "$(jq -er .bundledServices.objectStoreClient "$manifest")"; do
    docker pull "$reference" >/dev/null
  done

  mkdir -p "$certificate_dir"
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
    skopeo copy --all "oci-archive:$release_directory/$archive" "docker://$repository:qualification-$mode" >/dev/null
    actual="sha256:$(skopeo inspect --raw "docker://$reference" | sha256sum | awk '{print $1}')"
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

if [[ "$phase" == install ]]; then
  [[ ! -e "$HOME/.config/secondbox/config.json" ]] || { echo 'qualified guest is not a clean CLI host' >&2; exit 1; }
  [[ -z "$(find "$HOME" -maxdepth 1 -type d -name 'secondbox-install_*' -print -quit)" ]] || { echo 'qualified guest has a prior installer operation' >&2; exit 1; }
  [[ -z "$(docker ps -a --filter label=com.docker.compose.project --format '{{.Names}}' | grep secondbox || true)" ]] || { echo 'qualified guest has prior SecondBox containers' >&2; exit 1; }

  if [[ "$mode" == existing_reflink_filesystem ]]; then
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
    '{operation:$operation,operationId:$operationId,workspace:$workspace,artifacts:$artifacts,manifest:$manifest,cliBinary:$cliBinary,cliConfig:$cliConfig,artifactFingerprint:$artifactFingerprint,filesystemIdentity:$filesystemIdentity,neighbor:$neighbor}' >"$state"
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

if [[ "$mode" == existing_reflink_filesystem ]]; then
  isolation="$workspace/.qualification-reflink"
  sudo install -d -m 0700 -o root -g root -- "$isolation"
  printf 'source\n' | sudo tee -- "$isolation/source" >/dev/null
  sudo cp --reflink=always -- "$isolation/source" "$isolation/copy"
  printf 'copy changed\n' | sudo tee -- "$isolation/copy" >/dev/null
  [[ "$(sudo cat -- "$isolation/source")" == source ]]
  jq -e '.storage.choice == "existing_mount" and (.storage.existingDeviceIdentity | length > 0) and ([.paths[].path | select(startswith("/dev"))] | length == 0)' "$plan" >/dev/null
  sudo mount -t tmpfs -o size=1m,nosuid,nodev,noexec tmpfs /srv/secondbox-dedicated
  if "$deploy" --accessible install --resume "$operation" --candidate-directory "$release_directory" >"$qualification_root/unsafe-filesystem-${mode}.log" 2>&1; then
    echo 'resume accepted a replacement filesystem at the reviewed existing mount' >&2
    exit 1
  fi
  sudo umount /srv/secondbox-dedicated
fi

"$deploy" --accessible install --resume "$operation" --candidate-directory "$release_directory" >"$qualification_root/resume-${mode}.log" 2>&1
jq -e '.status == "succeeded" and .completedStages[-1].stage == "smoke_execution"' "$receipt" >/dev/null
[[ "$(artifact_fingerprint "$artifacts")" == "$(jq -er .artifactFingerprint "$state")" ]] || { echo 'verified artifact bundle was unexpectedly re-extracted' >&2; exit 1; }
compose_project="$(jq -er '.completedStages[] | select(.stage == "compose_started") | .evidence.composeProject' "$receipt")"
[[ "$(docker ps --filter "label=com.docker.compose.project=$compose_project" --format '{{.ID}}' | wc -l)" -ge 4 ]]
jq -e '.completedStages[] | select(.stage == "readiness") | .evidence.runnerState == "ready"' "$receipt" >/dev/null
jq -e '.completedStages[] | select(.stage == "cli_login")' "$receipt" >/dev/null
jq -e '.completedStages[] | select(.stage == "smoke_execution") | .evidence.output == "hello from a microVM" and .evidence.exitStatus == "0"' "$receipt" >/dev/null
sandbox_id="$(jq -er '.completedStages[] | select(.stage == "smoke_execution") | .evidence.sandboxId' "$receipt")"
SECONDBOX_CONFIG="$cli_config" "$cli_binary" --output plain exec "$sandbox_id" -- python3 -c 'print("hello after reboot")' | grep -Fx 'hello after reboot' >/dev/null
sandbox_before_document="$(SECONDBOX_CONFIG="$cli_config" "$cli_binary" --output json sandboxes get --path "sandboxId=$sandbox_id")"
sandbox_before="$(jq -cS '{id,profile,profileRevisionId,workspace}' <<<"$sandbox_before_document")"
generation_before="$(jq -er '.generation | select(type == "number" and . >= 1)' <<<"$sandbox_before_document")"

"$deploy" --accessible uninstall "$operation" >"$qualification_root/uninstall-${mode}.log" 2>&1
jq -e '.status == "uninstalled"' "$receipt" >/dev/null
[[ -d "$workspace" && -d "$artifacts" && -f "$manifest_path" ]]
"$deploy" --accessible install --resume "$operation" --candidate-directory "$release_directory" >"$qualification_root/resume-after-uninstall-${mode}.log" 2>&1
sandbox_after_document="$(SECONDBOX_CONFIG="$cli_config" "$cli_binary" --output json sandboxes get --path "sandboxId=$sandbox_id")"
sandbox_after="$(jq -cS '{id,profile,profileRevisionId,workspace}' <<<"$sandbox_after_document")"
generation_after="$(jq -er '.generation | select(type == "number" and . >= 1)' <<<"$sandbox_after_document")"
[[ "$sandbox_before" == "$sandbox_after" ]] || { printf 'retained smoke Sandbox lineage changed across uninstall/resume: before=%s after=%s\n' "$sandbox_before" "$sandbox_after" >&2; exit 1; }
(( generation_after >= generation_before )) || { printf 'retained smoke Sandbox generation regressed across uninstall/resume: before=%s after=%s\n' "$generation_before" "$generation_after" >&2; exit 1; }
"$deploy" --accessible uninstall "$operation" >/dev/null 2>&1

sudo install -d -m 0755 "$workspace/.qualification-foreign-mount"
sudo mount -t tmpfs -o size=1m,nosuid,nodev,noexec tmpfs "$workspace/.qualification-foreign-mount"
if printf 'PURGE %s\n' "$operation_id" | "$deploy" --accessible uninstall --purge "$operation" >"$qualification_root/foreign-purge-${mode}.log" 2>&1; then
  echo 'purge accepted an unrecorded nested mount' >&2
  exit 1
fi
sudo mountpoint -q "$workspace/.qualification-foreign-mount"
sudo umount "$workspace/.qualification-foreign-mount"
sudo rmdir "$workspace/.qualification-foreign-mount"
mapfile -t purge_paths < <(jq -er '.createdResources[] | select(.id != "operation-directory" and (.path | type) == "string" and (.path | length) > 0) | .path' "$receipt")
printf 'PURGE %s\n' "$operation_id" | "$deploy" --accessible uninstall --purge "$operation" >"$qualification_root/purge-${mode}.log" 2>&1
jq -e '.status == "purged"' "$receipt" >/dev/null
[[ -f "$neighbor" && "$(<"$neighbor")" == 'preserve me' ]]
for purge_path in "${purge_paths[@]}"; do
  [[ ! -e "$purge_path" ]] || { echo "purge retained a receipt-owned path: $purge_path" >&2; exit 1; }
done
[[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$compose_project")" ]]
[[ -z "$(docker volume ls -q --filter "label=com.docker.compose.project=$compose_project")" ]]
[[ -z "$(docker network ls -q --filter "label=com.docker.compose.project=$compose_project")" ]]
[[ -f "$operation/install-plan.json" && -f "$receipt" ]]

common=(clean_host read_only_preflight bootstrap_checksum guided_install reboot_recovery mount_recovery compose_ready runner_ready cli_login hello_microvm stage_interrupt_resume verified_bundle_not_reextracted uninstall_workspace_preserved resume_same_sandbox_lineage purge_exact_resources purge_neighbor_preserved purge_foreign_mount_refused)
if [[ "$mode" == existing_reflink_filesystem ]]; then
  common+=(existing_reflink_isolation unsafe_filesystem_refusals)
fi
jq -n --arg mode "$mode" --arg filesystemIdentity "$(jq -er .filesystemIdentity "$state")" \
  --arg candidateManifestDigest "sha256:$(sha256sum "$manifest" | awk '{print $1}')" --argjson assertions "$(assertions_json "${common[@]}")" \
  '{mode:$mode,passed:true,rebootPassed:true,candidateManifestDigest:$candidateManifestDigest,filesystemIdentity:$filesystemIdentity,assertions:$assertions}' >"$evidence"
chmod 0644 "$evidence"
