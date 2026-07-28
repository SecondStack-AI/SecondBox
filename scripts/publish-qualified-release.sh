#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 3 ]]; then
  echo "Usage: scripts/publish-qualified-release.sh COMMAND WORKFLOW_RUN_EVENT.json RELEASE_EVIDENCE.json" >&2
  exit 2
fi

publication_command="$1"
workflow_run_event="$2"
release_evidence="$3"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
expected_repository="SecondStack-AI/SecondBox"
npm_package="@secondstack-ai/secondbox"
npm_registry="https://registry.npmjs.org"
publication_working_directory="$(mktemp -d)"

cleanup_publication_working_directory() {
  rm -rf -- "$publication_working_directory"
}
trap cleanup_publication_working_directory EXIT

for required_command in \
  awk cmp curl docker find gh git go grep head install jq node npm openssl realpath sed sha256sum tar; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox qualified publication requires command: $required_command" >&2
    exit 1
  fi
done

"$repo_root/scripts/verify-release-publication-eligibility.sh" "$release_evidence"
publication_identity="$publication_working_directory/publication-identity.json"
node "$repo_root/scripts/verify-qualified-publication-inputs.mjs" \
  "$workflow_run_event" \
  "$release_evidence" >"$publication_identity"

release_version="$(jq -er '.releaseVersion' "$publication_identity")"
release_tag="$(jq -er '.releaseTag' "$publication_identity")"
source_commit="$(jq -er '.sourceCommit' "$publication_identity")"
evidence_directory="$(cd "$(dirname "$release_evidence")" && pwd)"

publication_fail() {
  echo "SecondBox qualified publication failed: $*" >&2
  exit 1
}

publication_subject_field() {
  local subject_id="$1"
  local field="$2"
  jq -er --arg subject_id "$subject_id" \
    ".subjects[\$subject_id].$field" \
    "$publication_identity"
}

verify_publication_source_is_current_main() {
  local current_main
  if [[ "$(git -C "$repo_root" rev-parse HEAD)" != "$source_commit" ]]; then
    publication_fail "checked-out commit does not match the qualified source commit"
  fi
  current_main="$(
    git ls-remote \
      "https://github.com/$expected_repository.git" \
      refs/heads/main |
      awk '{print $1}'
  )"
  if [[ "$current_main" != "$source_commit" ]]; then
    publication_fail "qualified source commit is no longer current protected main"
  fi
}

inspect_registry_image_digest() {
  local image_reference="$1"
  local inspection
  local digest
  inspection="$(docker buildx imagetools inspect "$image_reference" 2>&1)" ||
    return 1
  digest="$(sed -n 's/^Digest:[[:space:]]*//p' <<<"$inspection" | head -n 1)"
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
  printf '%s\n' "$digest"
}

publish_exact_ghcr_tag() {
  local subject_id="$1"
  local source_reference
  local expected_digest
  local image_repository
  local versioned_reference
  local existing_output
  local existing_digest

  source_reference="$(publication_subject_field "$subject_id" locator)"
  expected_digest="sha256:$(publication_subject_field "$subject_id" sha256)"
  image_repository="${source_reference%@sha256:*}"
  versioned_reference="$image_repository:$release_tag"

  if [[ "$(inspect_registry_image_digest "$source_reference")" != "$expected_digest" ]]; then
    publication_fail "$subject_id registry source does not resolve to its qualified digest"
  fi

  if existing_output="$(docker buildx imagetools inspect "$versioned_reference" 2>&1)"; then
    existing_digest="$(
      sed -n 's/^Digest:[[:space:]]*//p' <<<"$existing_output" | head -n 1
    )"
    if [[ "$existing_digest" != "$expected_digest" ]]; then
      publication_fail "$versioned_reference already exists at a different digest"
    fi
    echo "SecondBox qualified publication kept exact existing image tag $versioned_reference"
    return
  fi
  if ! grep -Eqi 'manifest unknown|name unknown|not found|404' <<<"$existing_output"; then
    publication_fail "could not prove $versioned_reference is absent: $existing_output"
  fi

  docker buildx imagetools create \
    --tag "$versioned_reference" \
    "$source_reference"
  if [[ "$(inspect_registry_image_digest "$versioned_reference")" != "$expected_digest" ]]; then
    publication_fail "$versioned_reference did not publish the qualified digest"
  fi
  echo "SecondBox qualified publication created exact image tag $versioned_reference"
}

publish_qualified_ghcr_tags() {
  [[ "${GITHUB_REPOSITORY:?set GITHUB_REPOSITORY}" == "$expected_repository" ]] ||
    publication_fail "GITHUB_REPOSITORY must be $expected_repository"
  : "${GITHUB_ACTOR:?set GITHUB_ACTOR}"
  : "${GITHUB_TOKEN:?set GITHUB_TOKEN}"
  printf '%s' "$GITHUB_TOKEN" |
    docker login ghcr.io --username "$GITHUB_ACTOR" --password-stdin
  publish_exact_ghcr_tag control-plane-image
  publish_exact_ghcr_tag runner-image
  publish_exact_ghcr_tag guest-artifact-image
}

npm_version_metadata() {
  npm view "$npm_package@$release_version" --json --registry="$npm_registry"
}

verify_published_npm_package() {
  local expected_package
  local expected_digest
  local npm_metadata
  local npm_pack_result
  local downloaded_package

  expected_package="$evidence_directory/$(publication_subject_field typescript-sdk-package locator)"
  expected_digest="$(publication_subject_field typescript-sdk-package sha256)"
  npm_metadata="$(npm_version_metadata)" ||
    publication_fail "$npm_package@$release_version is not publicly readable"
  if ! jq -e '
    .name == "@secondstack-ai/secondbox" and
    (.dist.attestations.url | type == "string" and length > 0)
  ' <<<"$npm_metadata" >/dev/null; then
    publication_fail "$npm_package@$release_version has no public provenance attestation"
  fi

  install -d -m 0700 "$publication_working_directory/npm-download"
  npm_pack_result="$publication_working_directory/npm-pack.json"
  npm pack \
    "$npm_package@$release_version" \
    --ignore-scripts \
    --json \
    --pack-destination "$publication_working_directory/npm-download" \
    --registry="$npm_registry" >"$npm_pack_result"
  downloaded_package="$publication_working_directory/npm-download/$(
    jq -er '.[0].filename' "$npm_pack_result"
  )"
  if [[ "$(sha256sum "$downloaded_package" | awk '{print $1}')" != "$expected_digest" ||
        "$(sha256sum "$expected_package" | awk '{print $1}')" != "$expected_digest" ]]; then
    publication_fail "$npm_package@$release_version bytes differ from the qualified package"
  fi
}

publish_qualified_npm_package() {
  local expected_package
  local metadata_error
  local npm_version
  local npm_major
  local npm_minor
  local npm_patch

  [[ "${RUNNER_ENVIRONMENT:?set RUNNER_ENVIRONMENT}" == "github-hosted" ]] ||
    publication_fail "npm trusted publication requires a GitHub-hosted runner"
  if [[ -n "${NODE_AUTH_TOKEN-}" || -n "${NPM_TOKEN-}" ]]; then
    publication_fail "npm trusted publication forbids persistent npm tokens"
  fi
  npm_version="$(npm --version)"
  IFS=. read -r npm_major npm_minor npm_patch <<<"$npm_version"
  if [[ ! "$npm_major" =~ ^[0-9]+$ ||
        ! "$npm_minor" =~ ^[0-9]+$ ||
        ! "$npm_patch" =~ ^[0-9]+$ ]] ||
     ((npm_major < 11)) ||
     ((npm_major == 11 && npm_minor < 5)) ||
     ((npm_major == 11 && npm_minor == 5 && npm_patch < 1)); then
    publication_fail "npm trusted publication requires npm 11.5.1 or newer"
  fi
  npm view "$npm_package" name --registry="$npm_registry" >/dev/null ||
    publication_fail "$npm_package must exist before its trusted publisher can be used"

  metadata_error="$publication_working_directory/npm-view-error.log"
  if npm_version_metadata >"$publication_working_directory/npm-existing.json" 2>"$metadata_error"; then
    verify_published_npm_package
    echo "SecondBox qualified publication kept exact existing npm package $npm_package@$release_version"
    return
  fi
  if ! grep -Eq 'E404|404 Not Found' "$metadata_error"; then
    publication_fail "could not prove $npm_package@$release_version is absent: $(<"$metadata_error")"
  fi

  expected_package="$evidence_directory/$(publication_subject_field typescript-sdk-package locator)"
  npm publish \
    "$expected_package" \
    --access public \
    --provenance \
    --registry="$npm_registry"
  verify_published_npm_package
  echo "SecondBox qualified publication published exact npm package $npm_package@$release_version"
}

verify_public_ghcr_subjects() {
  local anonymous_docker_config="$publication_working_directory/public-docker"
  local subject_id
  local source_reference
  local expected_digest
  local versioned_reference

  install -d -m 0700 "$anonymous_docker_config"
  for subject_id in control-plane-image runner-image guest-artifact-image; do
    source_reference="$(publication_subject_field "$subject_id" locator)"
    expected_digest="sha256:$(publication_subject_field "$subject_id" sha256)"
    versioned_reference="${source_reference%@sha256:*}:$release_tag"
    if [[ "$(
      DOCKER_CONFIG="$anonymous_docker_config" \
        inspect_registry_image_digest "$source_reference"
    )" != "$expected_digest" ||
          "$(
      DOCKER_CONFIG="$anonymous_docker_config" \
        inspect_registry_image_digest "$versioned_reference"
    )" != "$expected_digest" ]]; then
      publication_fail "$subject_id is not anonymously readable at the exact qualified digest"
    fi
  done
}

prepare_github_release_assets() {
  local asset_directory="$1"
  local subject_id
  local locator
  local release_archive
  local extracted_root
  local package_manifest
  local unit_relative_path
  local expected_unit_digest
  local evidence_artifact_record
  local evidence_artifact_path
  local evidence_artifact_digest
  local evidence_artifact_source
  local evidence_asset_name

  install -d -m 0700 "$asset_directory"
  for subject_id in \
    linux-release-package \
    secondbox \
    secondbox-artifact-evidence \
    secondbox-guest-agent \
    secondbox-runner \
    secondbox-runner-identity \
    secondboxd \
    guest-execution-bundle \
    go-sdk-package \
    typescript-sdk-package; do
    locator="$(publication_subject_field "$subject_id" locator)"
    install -m 0644 "$evidence_directory/$locator" "$asset_directory/$(basename "$locator")"
  done
  install -m 0644 \
    "$release_evidence" \
    "$evidence_directory/release-subjects.json" \
    "$evidence_directory/current-compatibility.json" \
    "$evidence_directory/signatures/release-subjects.sig" \
    "$evidence_directory/signatures/release-subjects.signing.pub" \
    "$evidence_directory/provenance.intoto.json" \
    "$asset_directory/"

  while IFS= read -r evidence_artifact_record; do
    evidence_artifact_path="$(jq -er '.path' <<<"$evidence_artifact_record")"
    evidence_artifact_digest="$(jq -er '.sha256' <<<"$evidence_artifact_record")"
    if [[ ! "$evidence_artifact_path" =~ ^[A-Za-z0-9._/-]+$ ||
          "$evidence_artifact_path" == /* ||
          "$evidence_artifact_path" == ../* ||
          "$evidence_artifact_path" == */../* ]]; then
      publication_fail "release evidence contains an unsafe artifact path: $evidence_artifact_path"
    fi
    evidence_artifact_source="$(realpath -e -- "$evidence_directory/$evidence_artifact_path")"
    if [[ "$evidence_artifact_source" != "$evidence_directory/$evidence_artifact_path" ||
          -L "$evidence_directory/$evidence_artifact_path" ||
          ! -f "$evidence_artifact_source" ||
          "$(sha256sum "$evidence_artifact_source" | awk '{print $1}')" != \
            "$evidence_artifact_digest" ]]; then
      publication_fail "release evidence artifact is unsafe or changed: $evidence_artifact_path"
    fi
    evidence_asset_name="evidence-${evidence_artifact_digest:0:16}-$(basename "$evidence_artifact_path")"
    if [[ -e "$asset_directory/$evidence_asset_name" ]]; then
      if ! cmp -s "$evidence_artifact_source" "$asset_directory/$evidence_asset_name"; then
        publication_fail "release evidence asset name collision: $evidence_asset_name"
      fi
      continue
    fi
    install -m 0644 \
      "$evidence_artifact_source" \
      "$asset_directory/$evidence_asset_name"
  done < <(
    jq -c '
      [.evidence | to_entries[].value.artifacts[]?]
      | unique_by(.path)
      | sort_by(.path)
      | .[]
    ' "$release_evidence"
  )

  release_archive="$asset_directory/secondbox-$release_version-linux-amd64.tar.gz"
  install -d -m 0700 "$publication_working_directory/release-archive"
  tar -xzf "$release_archive" -C "$publication_working_directory/release-archive"
  extracted_root="$publication_working_directory/release-archive/secondbox-$release_version-linux-amd64"
  package_manifest="$extracted_root/release-package-manifest.json"
  for unit_relative_path in \
    runner/deploy/secondbox-runner.service \
    runner/deploy/secondbox-runner-devnet.service; do
    expected_unit_digest="$(
      jq -er --arg path "$unit_relative_path" \
        '.files[] | select(.path == $path) | .sha256' \
        "$package_manifest"
    )"
    if [[ "$(sha256sum "$extracted_root/$unit_relative_path" | awk '{print $1}')" != \
          "$expected_unit_digest" ]]; then
      publication_fail "$unit_relative_path does not match the qualified archive manifest"
    fi
    install -m 0644 \
      "$extracted_root/$unit_relative_path" \
      "$asset_directory/$(basename "$unit_relative_path")"
  done
}

github_api_optional() {
  local output_path="$1"
  shift
  local error_path="$publication_working_directory/github-api-error.log"
  if gh api "$@" >"$output_path" 2>"$error_path"; then
    return 0
  fi
  if grep -Eq 'HTTP 404|Not Found' "$error_path"; then
    return 1
  fi
  publication_fail "GitHub API request failed: $(<"$error_path")"
}

write_expected_github_asset_names() {
  local asset_directory="$1"
  local output_path="$2"
  find "$asset_directory" -maxdepth 1 -type f -printf '%f\n' |
    LC_ALL=C sort |
    jq -Rsc 'split("\n") | map(select(length > 0))' >"$output_path"
}

fetch_github_release_inventory() {
  local releases_path="$1"
  gh api --paginate \
    "repos/$expected_repository/releases?per_page=100" \
    --slurp \
    --jq '[.[][]]' >"$releases_path"
}

fetch_github_release_assets() {
  local release_id="$1"
  local assets_path="$2"
  gh api --paginate \
    "repos/$expected_repository/releases/$release_id/assets?per_page=100" \
    --slurp \
    --jq '[.[][]]' >"$assets_path"
}

verify_github_immutable_release_configuration() {
  local configuration_path="$1"
  : "${SECONDBOX_RELEASE_CONFIGURATION_TOKEN:?set SECONDBOX_RELEASE_CONFIGURATION_TOKEN}"
  GH_TOKEN="$SECONDBOX_RELEASE_CONFIGURATION_TOKEN" \
    gh api \
      -H "Accept: application/vnd.github+json" \
      -H "X-GitHub-Api-Version: 2026-03-10" \
      "repos/$expected_repository/immutable-releases" \
      >"$configuration_path" ||
    publication_fail "could not prove repository immutable releases are enabled"
  jq -e '.enabled == true' "$configuration_path" >/dev/null ||
    publication_fail "repository immutable releases are not enabled"
}

verify_github_release_inventory() {
  local phase="$1"
  local immutable_configuration="$2"
  local expected_names="$3"
  local releases_path="$4"
  local assets_path="$5"
  node "$repo_root/scripts/verify-github-release-state.mjs" \
    "$immutable_configuration" \
    "$releases_path" \
    "$assets_path" \
    "$expected_names" \
    "$release_tag" \
    "$phase"
}

ensure_exact_github_tag() {
  local ref_record="$publication_working_directory/github-tag.json"
  if github_api_optional "$ref_record" \
    "repos/$expected_repository/git/ref/tags/$release_tag"; then
    [[ "$(jq -er '.object.type' "$ref_record")" == "commit" &&
       "$(jq -er '.object.sha' "$ref_record")" == "$source_commit" ]] ||
      publication_fail "$release_tag already exists at another Git object"
    return
  fi
  gh api \
    --method POST \
    "repos/$expected_repository/git/refs" \
    -f "ref=refs/tags/$release_tag" \
    -f "sha=$source_commit" >/dev/null
}

ensure_exact_github_asset() {
  local release_id="$1"
  local release_is_draft="$2"
  local asset_path="$3"
  local asset_name
  local asset_record
  local asset_id
  local downloaded_asset

  asset_name="$(basename "$asset_path")"
  asset_record="$(
    gh api --paginate "repos/$expected_repository/releases/$release_id/assets" |
      jq -sc --arg name "$asset_name" \
        '[.[][] | select(.name == $name)]'
  )"
  if [[ "$(jq 'length' <<<"$asset_record")" -gt 1 ]]; then
    publication_fail "GitHub release has duplicate asset $asset_name"
  fi
  if [[ "$(jq 'length' <<<"$asset_record")" -eq 1 ]]; then
    asset_id="$(jq -er '.[0].id' <<<"$asset_record")"
    downloaded_asset="$publication_working_directory/downloaded-$asset_id"
    gh api \
      -H "Accept: application/octet-stream" \
      "repos/$expected_repository/releases/assets/$asset_id" >"$downloaded_asset"
    [[ "$(sha256sum "$downloaded_asset" | awk '{print $1}')" == \
       "$(sha256sum "$asset_path" | awk '{print $1}')" ]] ||
      publication_fail "GitHub release asset $asset_name exists with different bytes"
    return
  fi
  [[ "$release_is_draft" == "true" ]] ||
    publication_fail "published GitHub release is missing immutable asset $asset_name"
  gh release upload "$release_tag" "$asset_path"
}

publish_qualified_github_release() {
  local release_record="$publication_working_directory/github-release.json"
  local release_notes="$publication_working_directory/release-notes.md"
  local asset_directory="$publication_working_directory/github-assets"
  local expected_asset_names="$publication_working_directory/expected-github-assets.json"
  local immutable_configuration="$publication_working_directory/immutable-release-configuration.json"
  local releases_path="$publication_working_directory/github-releases.json"
  local assets_path="$publication_working_directory/github-release-assets.json"
  local release_count
  local release_id
  local release_is_draft
  local asset_path

  [[ "${GITHUB_REPOSITORY:?set GITHUB_REPOSITORY}" == "$expected_repository" ]] ||
    publication_fail "GITHUB_REPOSITORY must be $expected_repository"
  : "${GITHUB_TOKEN:?set GITHUB_TOKEN}"
  verify_public_ghcr_subjects
  verify_published_npm_package
  prepare_github_release_assets "$asset_directory"
  write_expected_github_asset_names "$asset_directory" "$expected_asset_names"
  verify_github_immutable_release_configuration "$immutable_configuration"
  fetch_github_release_inventory "$releases_path"
  release_count="$(
    jq --arg release_tag "$release_tag" \
      '[.[] | select(.tag_name == $release_tag)] | length' \
      "$releases_path"
  )"
  if [[ "$release_count" == 1 ]]; then
    release_id="$(
      jq -er --arg release_tag "$release_tag" \
        '.[] | select(.tag_name == $release_tag) | .id' \
        "$releases_path"
    )"
    fetch_github_release_assets "$release_id" "$assets_path"
  else
    printf '[]\n' >"$assets_path"
  fi
  verify_github_release_inventory \
    before-upload \
    "$immutable_configuration" \
    "$expected_asset_names" \
    "$releases_path" \
    "$assets_path"

  cat >"$release_notes" <<EOF
SecondBox $release_tag

Source commit: $source_commit

Qualified images:
- $(publication_subject_field control-plane-image locator)
- $(publication_subject_field runner-image locator)
- $(publication_subject_field guest-artifact-image locator)

Every published byte and digest is bound by release-subjects.json and release-evidence.json.
EOF

  ensure_exact_github_tag
  if [[ "$release_count" == 0 ]]; then
    gh api \
      --method POST \
      "repos/$expected_repository/releases" \
      -f "tag_name=$release_tag" \
      -f "target_commitish=$source_commit" \
      -f "name=SecondBox $release_tag" \
      -F "body=@$release_notes" \
      -F "draft=true" \
      -F "prerelease=false" >"$release_record"
  fi
  fetch_github_release_inventory "$releases_path"
  release_id="$(
    jq -er --arg release_tag "$release_tag" '
      [.[] | select(.tag_name == $release_tag)] |
      if length == 1 then .[0].id else error("release is absent or duplicate") end
    ' "$releases_path"
  )"
  fetch_github_release_assets "$release_id" "$assets_path"
  verify_github_release_inventory \
    before-upload \
    "$immutable_configuration" \
    "$expected_asset_names" \
    "$releases_path" \
    "$assets_path"
  jq -er --arg release_tag "$release_tag" \
    '.[] | select(.tag_name == $release_tag)' \
    "$releases_path" >"$release_record"
  release_id="$(jq -er '.id' "$release_record")"
  release_is_draft="$(jq -er '.draft' "$release_record")"
  if [[ "$(jq -er '.tag_name' "$release_record")" != "$release_tag" ||
        "$(jq -er '.name' "$release_record")" != "SecondBox $release_tag" ||
        "$(jq -er '.prerelease' "$release_record")" != "false" ]]; then
    publication_fail "GitHub release identity does not match the qualified stable release"
  fi

  while IFS= read -r asset_path; do
    ensure_exact_github_asset "$release_id" "$release_is_draft" "$asset_path"
  done < <(find "$asset_directory" -maxdepth 1 -type f -print | LC_ALL=C sort)
  fetch_github_release_inventory "$releases_path"
  fetch_github_release_assets "$release_id" "$assets_path"
  verify_github_release_inventory \
    after-upload \
    "$immutable_configuration" \
    "$expected_asset_names" \
    "$releases_path" \
    "$assets_path"

  if [[ "$release_is_draft" == "true" ]]; then
    gh api \
      --method PATCH \
      "repos/$expected_repository/releases/$release_id" \
      -F "draft=false" >"$release_record"
  fi
  [[ "$(jq -er '.draft' "$release_record")" == "false" ]] ||
    publication_fail "GitHub release did not publish"
}

verify_public_github_release() {
  local public_release="$publication_working_directory/public-release.json"
  local public_ref
  local release_id
  local asset_directory="$publication_working_directory/public-assets"
  local expected_assets="$publication_working_directory/expected-assets"
  local expected_asset_names="$publication_working_directory/expected-public-assets.json"
  local public_releases="$publication_working_directory/public-releases.json"
  local public_assets="$publication_working_directory/public-assets.json"
  local expected_asset
  local asset_name
  local asset_url
  local downloaded_asset

  curl --fail --silent --show-error \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2026-03-10" \
    "https://api.github.com/repos/$expected_repository/releases/tags/$release_tag" \
    >"$public_release"
  if [[ "$(jq -er '.draft' "$public_release")" != "false" ||
        "$(jq -er '.prerelease' "$public_release")" != "false" ||
        "$(jq -r '.immutable // false' "$public_release")" != "true" ]]; then
    publication_fail "GitHub release is not public, stable, and immutable"
  fi
  public_ref="$(
    git ls-remote \
      "https://github.com/$expected_repository.git" \
      "refs/tags/$release_tag" |
      awk '{print $1}'
  )"
  [[ "$public_ref" == "$source_commit" ]] ||
    publication_fail "public Git tag does not resolve to the qualified source commit"

  prepare_github_release_assets "$expected_assets"
  release_id="$(jq -er '.id' "$public_release")"
  write_expected_github_asset_names "$expected_assets" "$expected_asset_names"
  jq -s . "$public_release" >"$public_releases"
  fetch_github_release_assets "$release_id" "$public_assets"
  verify_github_release_inventory \
    public \
    - \
    "$expected_asset_names" \
    "$public_releases" \
    "$public_assets"
  install -d -m 0700 "$asset_directory"
  while IFS= read -r expected_asset; do
    asset_name="$(basename "$expected_asset")"
    asset_url="$(
      jq -er --arg name "$asset_name" \
        '.[] | select(.name == $name) | .browser_download_url' \
        "$public_assets"
    )"
    downloaded_asset="$asset_directory/$asset_name"
    curl --fail --silent --show-error --location \
      "$asset_url" \
      --output "$downloaded_asset"
    [[ "$(sha256sum "$downloaded_asset" | awk '{print $1}')" == \
       "$(sha256sum "$expected_asset" | awk '{print $1}')" ]] ||
      publication_fail "public GitHub release asset $asset_name has different bytes"
  done < <(find "$expected_assets" -maxdepth 1 -type f -print | LC_ALL=C sort)
  [[ "$release_id" =~ ^[0-9]+$ ]] ||
    publication_fail "public GitHub release id is invalid"
}

verify_public_go_module() {
  local module_record="$publication_working_directory/go-module.json"
  install -d -m 0700 "$publication_working_directory/go-module-probe"
  (
    cd "$publication_working_directory/go-module-probe"
    GOPROXY="https://proxy.golang.org" \
      GOSUMDB="sum.golang.org" \
      GOWORK="off" \
      go list -m -json \
        "github.com/SecondStack-AI/SecondBox@$release_tag"
  ) >"$module_record"
  [[ "$(jq -er '.Version' "$module_record")" == "$release_tag" ]] ||
    publication_fail "public Go module did not resolve the qualified release tag"
}

verify_qualified_public_release() {
  verify_public_ghcr_subjects
  verify_published_npm_package
  verify_public_github_release
  verify_public_go_module
  echo "SecondBox qualified public release $release_tag verified at $source_commit"
}

verify_publication_source_is_current_main

case "$publication_command" in
  publish-ghcr-tags)
    publish_qualified_ghcr_tags
    ;;
  publish-npm)
    publish_qualified_npm_package
    ;;
  publish-github-release)
    publish_qualified_github_release
    ;;
  verify-public-release)
    verify_qualified_public_release
    ;;
  *)
    echo "SecondBox qualified publication command must be publish-ghcr-tags, publish-npm, publish-github-release, or verify-public-release" >&2
    exit 2
    ;;
esac
