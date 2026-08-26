#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

if [[ "$#" -ne 6 ]]; then
  echo "usage: scripts/bootstrap-development-tenancy.sh SECONDBOX_CLI URL PLATFORM_TOKEN_FILE TENANT_REF SUBJECT_REF PROFILE" >&2
  exit 2
fi

cli_binary="$1"
endpoint="$2"
platform_token_file="$3"
tenant_ref="$4"
subject_ref="$5"
profile="$6"

[[ "$cli_binary" == /* && -x "$cli_binary" && ! -L "$cli_binary" ]] || { echo "development tenancy bootstrap requires an absolute executable CLI path" >&2; exit 1; }
[[ "$endpoint" == http://* || "$endpoint" == https://* ]] || { echo "development tenancy bootstrap requires an absolute HTTP endpoint" >&2; exit 1; }
[[ "$platform_token_file" == /* && -f "$platform_token_file" && ! -L "$platform_token_file" ]] || { echo "development tenancy bootstrap requires an absolute regular platform-token file" >&2; exit 1; }
for value in "$tenant_ref" "$subject_ref" "$profile"; do
  [[ -n "$value" && "$value" != *$'\n'* && "$value" != *$'\r'* ]] || { echo "development tenancy bootstrap references must be non-empty single-line values" >&2; exit 1; }
done

bootstrap_directory="$(mktemp -d)"
trap 'rm -rf -- "$bootstrap_directory"' EXIT
platform_config="$bootstrap_directory/platform.json"
controller_config="$bootstrap_directory/controller.json"
application_config="$bootstrap_directory/application.json"
expires_at="$(date -u -d '+24 hours' '+%Y-%m-%dT%H:%M:%SZ')"
platform_token="$(tr -d '\n' <"$platform_token_file")"

SECONDBOX_CONFIG="$platform_config" SECONDBOX_URL="$endpoint" SECONDBOX_TOKEN="$platform_token" \
  "$cli_binary" --output json platform login >/dev/null

jq -n \
  --arg ref "$tenant_ref" \
  --arg profile "$profile" \
  '{ref:$ref,allowedProfileGrants:[$profile],allowedApplicationScopes:["sandbox:read","sandbox:lifecycle","sandbox:exec","sandbox:files","sandbox:ports"],aggregateQuota:{maxSandboxes:10,maxActiveInstances:10,maxCpuMillis:20000,maxMemoryBytes:21474836480,maxSnapshots:20,maxPortSessions:20,maxConcurrentOperations:20,maxActiveSubjects:10,maxApplicationAuthorities:20},expiryPolicy:{maximumSubjectLifetimeSeconds:86400,maximumAuthorityLifetimeSeconds:86400},metadata:{bootstrap:"development-post-start"}}' \
  >"$bootstrap_directory/tenant.json"
SECONDBOX_CONFIG="$platform_config" "$cli_binary" --output json tenant create \
  --file "$bootstrap_directory/tenant.json" --idempotency-key "development-tenant-$tenant_ref" >/dev/null

jq -n --arg expiresAt "$expires_at" '{expiresAt:$expiresAt,metadata:{bootstrap:"development-post-start"}}' \
  >"$bootstrap_directory/controller.json"
controller_response="$(SECONDBOX_CONFIG="$platform_config" "$cli_binary" --output json tenant controller-authority create \
  "$tenant_ref" --file "$bootstrap_directory/controller.json" --idempotency-key "development-controller-$tenant_ref")"
controller_token="$(jq -er '.bearerToken' <<<"$controller_response")"

SECONDBOX_CONFIG="$controller_config" SECONDBOX_URL="$endpoint" SECONDBOX_TOKEN="$controller_token" \
  "$cli_binary" --output json controller login >/dev/null
jq -n \
  --arg ref "$subject_ref" \
  '{ref:$ref,quota:{maxSandboxes:10,maxActiveInstances:10,maxCpuMillis:20000,maxMemoryBytes:21474836480,maxSnapshots:20,maxPortSessions:20,maxConcurrentOperations:20},metadata:{bootstrap:"development-post-start"}}' \
  >"$bootstrap_directory/subject.json"
SECONDBOX_CONFIG="$controller_config" "$cli_binary" --output json subject create \
  --file "$bootstrap_directory/subject.json" --idempotency-key "development-subject-$tenant_ref-$subject_ref" >/dev/null

jq -n \
  --arg subjectRef "$subject_ref" \
  --arg profile "$profile" \
  --arg expiresAt "$expires_at" \
  '{subjectRef:$subjectRef,scopes:["sandbox:read","sandbox:lifecycle","sandbox:exec","sandbox:files","sandbox:ports"],profileGrants:[$profile],expiresAt:$expiresAt,metadata:{bootstrap:"development-post-start"}}' \
  >"$bootstrap_directory/application.json"
application_response="$(SECONDBOX_CONFIG="$controller_config" "$cli_binary" --output json application-authority create \
  --file "$bootstrap_directory/application.json" --idempotency-key "development-application-$tenant_ref-$subject_ref")"
application_token="$(jq -er '.bearerToken' <<<"$application_response")"

SECONDBOX_CONFIG="$application_config" SECONDBOX_URL="$endpoint" SECONDBOX_TOKEN="$application_token" \
  SECONDBOX_TENANT_REF="$tenant_ref" SECONDBOX_SUBJECT_REF="$subject_ref" \
  "$cli_binary" --output json application login >/dev/null
SECONDBOX_CONFIG="$application_config" "$cli_binary" --output json sandboxes list --query limit=1 >/dev/null

jq -n \
  --arg tenantRef "$tenant_ref" \
  --arg subjectRef "$subject_ref" \
  --arg controllerAuthorityId "$(jq -er '.authority.id' <<<"$controller_response")" \
  --arg controllerBearerToken "$controller_token" \
  --arg applicationAuthorityId "$(jq -er '.authority.id' <<<"$application_response")" \
  --arg applicationBearerToken "$application_token" \
  '{tenantRef:$tenantRef,subjectRef:$subjectRef,controllerAuthorityId:$controllerAuthorityId,controllerBearerToken:$controllerBearerToken,applicationAuthorityId:$applicationAuthorityId,applicationBearerToken:$applicationBearerToken,sandboxRequestAuthenticated:true}'
