#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
"$repo_root/scripts/test-prepare-gvisor-qualification.sh"
workflow="$repo_root/.github/workflows/release.yml"
stager="$repo_root/scripts/release-stage.sh"
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

if rg -q 'qualif|/dev/kvm|test-scenario|self-hosted' "$repo_root/.github/workflows"; then
  echo "GitHub workflows contain a forbidden qualification step" >&2
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

for dockerfile in "$repo_root/Dockerfile" "$repo_root/deploy/installer-tools.Dockerfile" "$repo_root/runner/Dockerfile" "$repo_root/runner/deploy/microvm-artifact-transport.Dockerfile" "$repo_root/runner/Dockerfile.gvisor" "$repo_root/runner/deploy/gvisor-artifact-transport.Dockerfile" "$repo_root/deploy/host-cli.Dockerfile"; do
  while IFS= read -r base; do
    [[ "$base" == "scratch" || "$base" == *@sha256:* ]] || { echo "release Dockerfile uses mutable base $base" >&2; exit 1; }
  done < <(awk '$1 == "FROM" {for (i = 2; i <= NF; i++) if ($i !~ /^--/) {print $i; break}}' "$dockerfile")
done

if rg -n '^\s*(-\s+)?uses:\s*[^ ]+@(main|master|v[0-9]+([.]?[0-9]+)*)\s*$' "$workflow"; then
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
  # The publisher starts only once the draft holds both artifact sets.
  "$uploader" 1.2.3 "$fixture/output"
  if rg -q '^workflow run' "$RELEASE_TEST_STATE/calls"; then echo 'release upload dispatched the publisher with one artifact set' >&2; exit 1; fi
  "$uploader" 1.2.3 "$fixture/output-arm64"
  test "$(rg -c '^workflow run release.yml --ref main -f version=1.2.3$' "$RELEASE_TEST_STATE/calls")" = 1
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
  if GH_TOKEN=fixture GITHUB_ACTOR=fixture "$publisher" 1.2.3 "$fixture/output"; then
    echo 'release publisher accepted a release without the arm64 artifact set' >&2; exit 1
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
)
echo 'Release upload and publication notes regression checks passed.'
