#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
"$repo_root/scripts/test-prepare-gvisor-qualification.sh"
workflow="$repo_root/.github/workflows/release.yml"
tag_workflow="$repo_root/.github/workflows/release-tag.yml"
stager="$repo_root/scripts/release-stage.sh"
shared_builder="$repo_root/scripts/release-build-shared.sh"
uploader="$repo_root/scripts/release-upload.sh"
publisher="$repo_root/scripts/release-publish.sh"

bash -n "$uploader"
bash -n "$publisher"

rg -q 'workflow_dispatch:' "$workflow"
rg -q 'runs-on: ubuntu-latest' "$workflow"
rg -q 'scripts/release-publish.sh' "$workflow"
rg -q -- '--tag latest --provenance' "$publisher"
rg -q -- '--draft=false' "$publisher"
rg -q -- '--prerelease=false' "$publisher"
rg -q -- '--latest' "$publisher"
rg -q 'gh workflow run release.yml' "$uploader"
rg -q 'qualification-evidence' "$stager"
rg -q '^export LC_ALL=C$' "$stager"

# Only the tag workflow runs on the self-hosted release runners. It builds the
# shared files once on a hosted runner, stages both artifact sets in parallel
# from them, uploads the arm64 set first, and leaves publication to the hosted
# publisher.
if rg -q 'qualif|/dev/kvm|test-scenario|self-hosted' "$workflow" "$repo_root/.github/workflows/ci.yml"; then
  echo "GitHub-hosted workflows contain a forbidden qualification step" >&2
  exit 1
fi
rg -q "tags: \['v\[0-9\]\*'\]" "$tag_workflow"
# Job graph: name, runner and needs of every job, in file order.
diff -u <(printf '%s\n' \
  $'build-shared\tubuntu-latest\t' \
  $'stage-amd64\t[self-hosted, Linux, X64, secondbox-release-amd64]\tbuild-shared' \
  $'stage-arm64\t[self-hosted, Linux, ARM64, secondbox-release-arm64]\tbuild-shared' \
  $'upload-arm64\t[self-hosted, Linux, ARM64, secondbox-release-arm64]\t[build-shared, stage-amd64, stage-arm64]' \
  $'upload-amd64\t[self-hosted, Linux, X64, secondbox-release-amd64]\t[build-shared, upload-arm64]') \
  <(awk '/^jobs:$/ {jobs = 1; next}
    jobs && /^  [a-z0-9-]+:$/ {if (job != "") print job "\t" runs "\t" needs; job = substr($1, 1, length($1) - 1); runs = ""; needs = ""; next}
    jobs && /^    runs-on: / {runs = substr($0, 14)}
    jobs && /^    needs: / {needs = substr($0, 12)}
    END {if (job != "") print job "\t" runs "\t" needs}' "$tag_workflow") || {
  echo "tag workflow job graph differs from build-shared, parallel staging, then arm64 and amd64 uploads" >&2
  exit 1
}
rg -q 'run: scripts/release-build-shared.sh "\$RELEASE_VERSION" "\$RUNNER_TEMP/shared-\$RELEASE_VERSION"$' "$tag_workflow"
rg -q '/usr/bin/just release "\$RELEASE_VERSION" "\$RELEASE_SHARED"$' "$tag_workflow"
rg -q '/usr/bin/just release-arm64 "\$RELEASE_VERSION" "\$RELEASE_SHARED"$' "$tag_workflow"
# A tar keeps binary modes across the workflow artifact, in and out.
rg -q 'tar -C "\$RUNNER_TEMP/shared-\$RELEASE_VERSION" -cf "\$RUNNER_TEMP/shared.tar" \.$' "$tag_workflow"
test "$(rg -c 'tar -C "\$RELEASE_SHARED" -xpf "\$RUNNER_TEMP/shared-download/shared.tar"$' "$tag_workflow")" = 2
# The shared build needs no KVM, Docker or host configuration.
if rg -q 'qualif|/dev/kvm|test-scenario|docker (buildx|build|run)|release\.env|RELEASE_' "$shared_builder"; then
  echo "shared release build depends on a release runner" >&2
  exit 1
fi
# Tag-derived values reach shell steps only through the environment.
if awk '/^ +(- )?run:/ {block = 1; indent = match($0, /[^ -]/); if ($0 ~ /\$\{\{/) found = 1; next}
  block && match($0, /[^ ]/) > indent && $0 ~ /\$\{\{/ {found = 1}
  block && match($0, /[^ ]/) <= indent {block = 0}
  END {exit !found}' "$tag_workflow"; then
  echo "tag workflow interpolates expressions into shell source" >&2
  exit 1
fi
test "$(rg -c 'scripts/release-upload.sh' "$tag_workflow")" = 2
test "$(rg -c "if: \\$\\{\\{ !contains\\(github.ref_name, '-dryrun.'\\) \\}\\}" "$tag_workflow")" = 2
if rg -q 'release-publish|npm publish|skopeo|id-token|packages: write' "$tag_workflow"; then
  echo "tag workflow publishes instead of dispatching the hosted publisher" >&2
  exit 1
fi
if rg -q 'qualif|/dev/kvm|test-scenario|self-hosted' "$uploader" "$publisher"; then
  echo "hosted release publication contains a forbidden qualification step" >&2
  exit 1
fi
if rg -q 'qualification-attestation|attest-build-provenance|candidate-evidence|publication-input|release-index|verify-publication' \
  "$stager" "$workflow" "$uploader" "$publisher"; then
  echo "release flow contains a removed candidate, attestation, or finalization surface" >&2
  exit 1
fi
if rg -q 'release-stage|docker build|go build|npm pack' "$workflow"; then
  echo "GitHub publisher rebuilds locally supplied artifacts" >&2
  exit 1
fi
if rg -q 'qualification-attestation|release-index|candidate-evidence|publication-input|verify-publication' \
  "$repo_root/cmd/secondbox-deploy/main.go" "$repo_root/cmd/secondbox-release-tool/main.go"; then
  echo "release CLIs retain a removed candidate or finalization command" >&2
  exit 1
fi
test ! -e "$repo_root/pkg/releasefinalize"
test ! -e "$repo_root/pkg/releasepublish"

for dockerfile in "$repo_root/Dockerfile" "$repo_root/deploy/installer-tools.Dockerfile" "$repo_root/runner/Dockerfile" "$repo_root/runner/deploy/microvm-artifact-transport.Dockerfile" "$repo_root/runner/Dockerfile.gvisor" "$repo_root/runner/deploy/gvisor-artifact-transport.Dockerfile"; do
  while IFS= read -r base; do
    [[ "$base" == "scratch" || "$base" == *@sha256:* ]] || { echo "release Dockerfile uses mutable base $base" >&2; exit 1; }
  done < <(awk '$1 == "FROM" {print $2}' "$dockerfile")
done

if rg -n '^\s*(-\s+)?uses:\s*[^ ]+@(main|master|v[0-9]+([.]?[0-9]+)*)\s*$' "$workflow" "$tag_workflow"; then
  echo "release workflow contains an unpinned external action" >&2
  exit 1
fi

# Exercise the real upload/publish scripts against a local draft transport.
# The fixture repository distinguishes immutable tag notes from checkout edits.
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/bin" "$fixture/repo/docs/releases" "$fixture/output" "$fixture/output-arm64" "$fixture/both" "$fixture/state"
export RELEASE_TEST_STATE="$fixture/state"
cat >"$fixture/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$RELEASE_TEST_STATE/calls"
case "$1 $2" in
  'auth status') ;;
  'release view')
    test -f "$RELEASE_TEST_STATE/draft"
    if [[ " $* " == *' --json assets '* ]]; then cat "$RELEASE_TEST_STATE/assets"
    elif [[ " $* " == *' --jq '* ]]; then cat "$RELEASE_TEST_STATE/draft"; fi
    ;;
  'release create'|'release edit')
    if [[ "$2" == create ]]; then echo true >"$RELEASE_TEST_STATE/draft"; : >"$RELEASE_TEST_STATE/assets"; fi
    while (($#)); do
      case "$1" in
        --notes-file) cp "$2" "$RELEASE_TEST_STATE/body"; shift ;;
        --notes) printf '%s' "$2" >"$RELEASE_TEST_STATE/body"; shift ;;
        --draft=false) echo false >"$RELEASE_TEST_STATE/draft" ;;
      esac
      shift
    done
    ;;
  'release upload')
    for file in "${@:4}"; do [[ "$file" == --clobber ]] || basename "$file" >>"$RELEASE_TEST_STATE/assets"; done
    ;;
  'release delete-asset'|'workflow run') ;;
  'run list') if [[ -f "$RELEASE_TEST_STATE/runs-fail" ]]; then exit 1; fi; cat "$RELEASE_TEST_STATE/runs" 2>/dev/null || true ;;
  *) echo "unexpected gh invocation" >&2; exit 1 ;;
esac
GH
cat >"$fixture/bin/skopeo" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == login ]]; then cat >/dev/null; fi
if [[ "$1" == copy ]]; then test -f "${3#oci-archive:}"; printf 'skopeo %s %s\n' "$(basename "${3#oci-archive:}")" "$4" >>"$RELEASE_TEST_STATE/calls"; fi
SH
cat >"$fixture/bin/npm" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == view ]]
SH
chmod +x "$fixture/bin/"*
export PATH="$fixture/bin:$PATH"
(
  cd "$fixture/repo"
  git init -q
  git config user.name 'Release test'
  git config user.email 'release-test@example.invalid'
  printf '# Tagged notes\n\nDeployment boundary and bundle identity.\n' >docs/releases/v1.2.3.md
  git add docs/releases/v1.2.3.md
  git -c commit.gpgsign=false commit -qm fixture
  git tag v1.2.3
  git tag v1.2.4
  printf 'uncommitted wrong notes\n' >docs/releases/v1.2.3.md
  printf '{}\n' >"$fixture/output/secondbox-1.2.3-artifact-manifest.json"
  printf '{}\n' >"$fixture/output/secondbox-1.2.4-artifact-manifest.json"
  printf '{}\n' >"$fixture/output-arm64/secondbox-1.2.3-arm64-artifact-manifest.json"
  cp "$fixture/output/secondbox-1.2.3-artifact-manifest.json" "$fixture/output-arm64/secondbox-1.2.3-arm64-artifact-manifest.json" "$fixture/both/"
  for image in control-plane runner installer-tools microvm-artifacts runner-gvisor gvisor-artifacts; do printf '%s amd64\n' "$image" >"$fixture/both/$image.oci.tar"; done
  for image in control-plane installer-tools microvm-artifacts; do printf '%s arm64\n' "$image" >"$fixture/both/$image-arm64.oci.tar"; done
  (cd "$fixture/both" && ls | jq -R . | jq -s '{schemaVersion:1,files:(. + ["candidate-allowlist.json"])}' >candidate-allowlist.json)
  printf '%s\n' '{"files":["candidate-allowlist-arm64.json","secondbox-1.2.3-arm64-artifact-manifest.json","runner-arm64.oci.tar"]}' >"$fixture/both/candidate-allowlist-arm64.json"
  if "$uploader" 1.2.3 "$fixture/both"; then
    echo 'release upload accepted a directory with both artifact sets' >&2; exit 1
  fi
  # An optional arm64 set is uploaded first; the amd64 upload dispatches the publisher.
  "$uploader" 1.2.3 "$fixture/output-arm64"
  if rg -q '^workflow run' "$RELEASE_TEST_STATE/calls"; then echo 'release upload dispatched the publisher without the amd64 set' >&2; exit 1; fi
  "$uploader" 1.2.3 "$fixture/output"
  test "$(rg -c '^workflow run release.yml --ref main -f version=1.2.3$' "$RELEASE_TEST_STATE/calls")" = 1
  echo in_progress >"$RELEASE_TEST_STATE/runs"
  if "$uploader" 1.2.3 "$fixture/output-arm64"; then
    echo 'release upload accepted an arm64 set while the publisher runs' >&2; exit 1
  fi
  rm "$RELEASE_TEST_STATE/runs"
  touch "$RELEASE_TEST_STATE/runs-fail"
  if "$uploader" 1.2.3 "$fixture/output-arm64"; then
    echo 'release upload accepted an arm64 set without knowing the publisher state' >&2; exit 1
  fi
  rm "$RELEASE_TEST_STATE/runs-fail"
  # Recovery after a failed publication restores the arm64 set, then amd64.
  "$uploader" 1.2.3 "$fixture/output-arm64"
  rg -q '^# Tagged notes$' "$RELEASE_TEST_STATE/body"
  if rg -q 'uncommitted wrong notes' "$RELEASE_TEST_STATE/body"; then echo 'release upload used checkout notes' >&2; exit 1; fi
  rg -q -F 'releases/download/v1.2.3/install.sh | sh' "$RELEASE_TEST_STATE/body"
  rg -q -F '`npm install @secondstack-ai/secondbox@1.2.3`' "$RELEASE_TEST_STATE/body"
  rg -q -F '`secondbox-1.2.3-arm64-artifact-manifest.json`' "$RELEASE_TEST_STATE/body"
  cp "$RELEASE_TEST_STATE/body" "$fixture/expected"
  "$uploader" 1.2.3 "$fixture/output"
  cmp "$fixture/expected" "$RELEASE_TEST_STATE/body"

  printf '# Explicit notes\n\nLiteral $HOME and `command` remain text.\n' >"$fixture/custom notes.md"
  rm "$RELEASE_TEST_STATE/draft"
  "$uploader" 1.2.3 "$fixture/output" "$fixture/custom notes.md"
  rg -q '^# Explicit notes$' "$RELEASE_TEST_STATE/body"
  cp "$RELEASE_TEST_STATE/body" "$fixture/expected"
  if "$uploader" 1.2.3 "$fixture/output" "$fixture/absent.md"; then
    echo 'release upload accepted absent explicit notes' >&2; exit 1
  fi
  cmp "$fixture/expected" "$RELEASE_TEST_STATE/body"
  if GH_TOKEN=fixture GITHUB_ACTOR=fixture "$publisher" 1.2.3 "$fixture/output-arm64"; then
    echo 'release publisher accepted a release without the amd64 artifact set' >&2; exit 1
  fi
  # Neither an incomplete set nor a candidate in either set publishes anything.
  : >"$RELEASE_TEST_STATE/calls"
  if GH_TOKEN=fixture GITHUB_ACTOR=fixture "$publisher" 1.2.3 "$fixture/both"; then
    echo 'release publisher accepted an incomplete arm64 artifact set' >&2; exit 1
  fi
  printf 'runner arm64\n' >"$fixture/both/runner-arm64.oci.tar"
  cp "$fixture/both/candidate-allowlist-arm64.json" "$fixture/arm64-allowlist.json"
  for malformed in '{"files":[]}' '{}' 'not json'; do
    printf '%s\n' "$malformed" >"$fixture/both/candidate-allowlist-arm64.json"
    if GH_TOKEN=fixture GITHUB_ACTOR=fixture "$publisher" 1.2.3 "$fixture/both"; then
      echo "release publisher accepted the allowlist $malformed" >&2; exit 1
    fi
  done
  cp "$fixture/arm64-allowlist.json" "$fixture/both/candidate-allowlist-arm64.json"
  printf '{"sourceCommit":"other"}\n' >"$fixture/both/secondbox-1.2.3-arm64-artifact-manifest.json"
  if GH_TOKEN=fixture GITHUB_ACTOR=fixture "$publisher" 1.2.3 "$fixture/both"; then
    echo 'release publisher accepted artifact sets of different commits' >&2; exit 1
  fi
  printf '{"openapi":{"digest":"other"}}\n' >"$fixture/both/secondbox-1.2.3-arm64-artifact-manifest.json"
  if GH_TOKEN=fixture GITHUB_ACTOR=fixture "$publisher" 1.2.3 "$fixture/both"; then
    echo 'release publisher accepted artifact sets pinning different shared files' >&2; exit 1
  fi
  printf '{}\n' >"$fixture/both/secondbox-1.2.3-arm64-artifact-manifest.json"
  printf '{"candidate":true}\n' >"$fixture/both/secondbox-1.2.3-artifact-manifest.json"
  if GH_TOKEN=fixture GITHUB_ACTOR=fixture "$publisher" 1.2.3 "$fixture/both"; then
    echo 'release publisher accepted an amd64 installer candidate beside a final arm64 set' >&2; exit 1
  fi
  if rg -q '^(skopeo|release edit|release delete-asset)' "$RELEASE_TEST_STATE/calls"; then
    echo 'release publisher changed the release before rejecting its input' >&2; exit 1
  fi
  printf '{}\n' >"$fixture/both/secondbox-1.2.3-artifact-manifest.json"
  GH_TOKEN=fixture GITHUB_ACTOR=fixture "$publisher" 1.2.3 "$fixture/both"
  diff -u <(printf '%s\n' \
    'skopeo control-plane.oci.tar docker://ghcr.io/secondstack-ai/secondbox/control-plane:v1.2.3' \
    'skopeo runner.oci.tar docker://ghcr.io/secondstack-ai/secondbox/runner:v1.2.3' \
    'skopeo installer-tools.oci.tar docker://ghcr.io/secondstack-ai/secondbox/installer-tools:v1.2.3' \
    'skopeo microvm-artifacts.oci.tar docker://ghcr.io/secondstack-ai/secondbox/microvm-artifacts:v1.2.3' \
    'skopeo runner-gvisor.oci.tar docker://ghcr.io/secondstack-ai/secondbox/runner-gvisor:v1.2.3' \
    'skopeo gvisor-artifacts.oci.tar docker://ghcr.io/secondstack-ai/secondbox/gvisor-artifacts:v1.2.3' \
    'skopeo control-plane-arm64.oci.tar docker://ghcr.io/secondstack-ai/secondbox/control-plane:v1.2.3-arm64' \
    'skopeo runner-arm64.oci.tar docker://ghcr.io/secondstack-ai/secondbox/runner:v1.2.3-arm64' \
    'skopeo installer-tools-arm64.oci.tar docker://ghcr.io/secondstack-ai/secondbox/installer-tools:v1.2.3-arm64' \
    'skopeo microvm-artifacts-arm64.oci.tar docker://ghcr.io/secondstack-ai/secondbox/microvm-artifacts:v1.2.3-arm64') \
    <(rg '^skopeo ' "$RELEASE_TEST_STATE/calls")
  test "$(cat "$RELEASE_TEST_STATE/draft")" = false
  cmp "$fixture/expected" "$RELEASE_TEST_STATE/body"
  if "$uploader" 1.2.3 "$fixture/output"; then
    echo 'release upload accepted a public release' >&2; exit 1
  fi
  cmp "$fixture/expected" "$RELEASE_TEST_STATE/body"

  rm "$RELEASE_TEST_STATE/draft"
  printf "checkout-only notes\n" >docs/releases/v1.2.4.md
  "$uploader" 1.2.4 "$fixture/output" ""
  rg -q '^Publishing locally built artifacts.$' "$RELEASE_TEST_STATE/body"
  rg -q -F '`npm install @secondstack-ai/secondbox@1.2.4`' "$RELEASE_TEST_STATE/body"
  if rg -q 'arm64-artifact-manifest' "$RELEASE_TEST_STATE/body"; then echo 'release notes name an absent arm64 set' >&2; exit 1; fi
  test "$(rg -c '^workflow run release.yml --ref main -f version=1.2.4$' "$RELEASE_TEST_STATE/calls")" = 1

  # An amd64-only release publishes the amd64 images alone.
  mkdir "$fixture/amd64-only"
  cp "$fixture/both/secondbox-1.2.3-artifact-manifest.json" "$fixture/amd64-only/"
  for image in control-plane runner installer-tools microvm-artifacts runner-gvisor gvisor-artifacts; do cp "$fixture/both/$image.oci.tar" "$fixture/amd64-only/"; done
  (cd "$fixture/amd64-only" && ls | jq -R . | jq -s '{schemaVersion:1,files:(. + ["candidate-allowlist.json"])}' >candidate-allowlist.json)
  echo true >"$RELEASE_TEST_STATE/draft"
  : >"$RELEASE_TEST_STATE/calls"
  GH_TOKEN=fixture GITHUB_ACTOR=fixture "$publisher" 1.2.3 "$fixture/amd64-only"
  test "$(rg -c '^skopeo .*:v1\.2\.3$' "$RELEASE_TEST_STATE/calls")" = 6
  if rg -q 'arm64' "$RELEASE_TEST_STATE/calls"; then echo 'amd64-only publication touched arm64 assets' >&2; exit 1; fi
  printf 'partial\n' >"$fixture/amd64-only/runner-arm64.oci.tar"
  if GH_TOKEN=fixture GITHUB_ACTOR=fixture "$publisher" 1.2.3 "$fixture/amd64-only"; then
    echo 'release publisher accepted arm64 set files without their manifest' >&2; exit 1
  fi
)
echo 'Release upload and publication notes regression checks passed.'
