#!/usr/bin/env bash
set -euo pipefail

# Builds the architecture-neutral files of a release exactly once: the OpenAPI
# document, Go module archive, TypeScript package, package metadata, SBOM, the
# secondbox and secondbox-deploy binaries of every host platform, and install.sh.
# shared-artifacts.json binds them to the version, tag and source commit; every
# guest architecture's staging consumes the directory through --shared-from.
# Nothing here needs KVM or host inputs, so release-tag.yml runs it on a
# GitHub-hosted runner.
export LC_ALL=C

usage() {
  echo "usage: scripts/release-build-shared.sh [--test-mode] VERSION OUTPUT_DIR" >&2
  exit 2
}

test_mode=false
while [[ "${1:-}" == --* ]]; do
  case "$1" in
    --test-mode) test_mode=true ;;
    *) usage ;;
  esac
  shift
done
[[ "$#" -eq 2 ]] || usage
version="$1"
output_dir="$2"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# The bundled PostgreSQL service image of every artifact set, reviewed in git.
readonly release_postgres_image='docker.io/library/postgres@sha256:882236b897e39051d2368c5ccc6cda944904723506b2dfc97f2a8f5bc9afa382'

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || {
  echo "release version must be SemVer without a v prefix or build metadata" >&2
  exit 1
}
if $test_mode; then
  source_commit="$("$repo_root/scripts/release-source-commit.sh" --test-mode)"
else
  source_commit="$("$repo_root/scripts/release-source-commit.sh")"
  [[ -z "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ]] || { echo "shared release build requires a clean repository" >&2; exit 1; }
  [[ "$(git -C "$repo_root" rev-parse --verify --quiet "refs/tags/v${version}^{commit}")" == "$source_commit" ]] || { echo "shared release build requires tag v${version} to identify HEAD ${source_commit}" >&2; exit 1; }
  git -C "$repo_root" merge-base --is-ancestor "$source_commit" refs/remotes/origin/main || { echo "tag v${version} is not on origin/main" >&2; exit 1; }
  [[ "$(GOTOOLCHAIN=local go -C "$repo_root" env GOVERSION)" == "go$(awk '$1 == "go" {print $2; exit}' "$repo_root/go.mod")" ]] || { echo "shared release build Go version differs from go.mod" >&2; exit 1; }
  export GOTOOLCHAIN=local
fi

[[ ! -e "$output_dir" && ! -L "$output_dir" ]] || { echo "shared release output already exists: $output_dir" >&2; exit 1; }
mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd)"
mkdir -p "$repo_root/.tmp"
temporary="$(mktemp -d "$repo_root/.tmp/release-build-shared.XXXXXX")"
cleanup() { rm -rf "$temporary"; }
trap cleanup EXIT

openapi_name="secondbox-${version}-openapi.json"
go_module_name="secondbox-${version}-go-module.tar.gz"
typescript_name="secondstack-ai-secondbox-${version}.tgz"
ldflags="-s -w -X github.com/SecondStack-AI/SecondBox/pkg/buildinfo.Version=${version} -X github.com/SecondStack-AI/SecondBox/pkg/buildinfo.SourceCommit=${source_commit}"

cp "$repo_root/contracts/openapi/v1/secondbox.openapi.json" "$output_dir/$openapi_name"

if $test_mode; then
  while IFS= read -r file; do [[ -f "$repo_root/$file" ]] && printf '%s\n' "$file"; done < <(git -C "$repo_root" ls-files --cached --others --exclude-standard | sort) | tar -C "$repo_root" --create --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner --files-from=- | gzip -n >"$output_dir/$go_module_name"
else
  git -C "$repo_root" archive --format=tar "$source_commit" | gzip -n >"$output_dir/$go_module_name"
fi
go -C "$repo_root" mod verify
go -C "$repo_root" list -m >/dev/null
module_smoke="$temporary/go-module"
mkdir "$module_smoke"
tar -xzf "$output_dir/$go_module_name" -C "$module_smoke"
go -C "$module_smoke" mod verify
go -C "$module_smoke" list ./sdk/go/secondboxclient >/dev/null

sdk_copy="$temporary/sdk-typescript"
# Qualification may replace dist concurrently; copy only SDK build inputs.
mkdir "$sdk_copy"
tar -C "$repo_root/sdk/typescript" --exclude=./dist --exclude=./node_modules -cf - . | tar -C "$sdk_copy" -xf -
"$repo_root/node_modules/.bin/tsc" -p "$sdk_copy/tsconfig.build.json"
npm --prefix "$sdk_copy" version "$version" --no-git-tag-version --ignore-scripts >/dev/null
npm pack "$sdk_copy" --pack-destination "$output_dir" >/dev/null
[[ -f "$output_dir/$typescript_name" ]] || { echo "TypeScript package name is not canonical" >&2; exit 1; }
while IFS= read -r packaged; do
  case "$packaged" in
    package/package.json|package/LICENSE|package/README.md|package/dist/*.js|package/dist/*.js.map|package/dist/*.d.ts|package/dist/*.d.ts.map) ;;
    *) echo "TypeScript package contains undeclared file: $packaged" >&2; exit 1 ;;
  esac
done < <(tar -tzf "$output_dir/$typescript_name")
node_project="$temporary/node-smoke"
mkdir "$node_project"
(cd "$node_project" && npm init --yes >/dev/null && npm install --ignore-scripts "$output_dir/$typescript_name" >/dev/null && node -e 'import("@secondstack-ai/secondbox").then(m => { if (typeof m.SecondBox !== "function") process.exit(1) })')

for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${platform%/*}"
  arch="${platform#*/}"
  for command in secondbox secondbox-deploy; do
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go -C "$repo_root" build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$output_dir/${command}_${version}_${os}_${arch}" "./cmd/$command"
    chmod 0755 "$output_dir/${command}_${version}_${os}_${arch}"
  done
done
host_platform="$(go env GOHOSTOS)_$(go env GOHOSTARCH)"
for command in secondbox secondbox-deploy; do
  "$output_dir/${command}_${version}_${host_platform}" version | jq -e --arg version "$version" --arg commit "$source_commit" '.version == $version and .sourceCommit == $commit' >/dev/null || {
    echo "$command binary release identity mismatch" >&2
    exit 1
  }
done
for binary in "$output_dir"/secondbox*_"${version}"_*; do
  if ! strings "$binary" | grep -Fx -- "$version" >/dev/null &&
     ! strings "$binary" | grep -Fx -- "_${version}" >/dev/null; then
    echo "binary lacks release version: $binary" >&2
    exit 1
  fi
  strings "$binary" | grep -Fx -- "$source_commit" >/dev/null || { echo "binary lacks source commit: $binary" >&2; exit 1; }
done
"$repo_root/scripts/generate-install-bootstrap.sh" "$version" "$(sha256sum "$output_dir/secondbox-deploy_${version}_linux_amd64" | awk '{print $1}')" "$output_dir/install.sh"

jq -n --arg version "$version" --arg commit "$source_commit" --arg ts "$typescript_name" --arg go "$go_module_name" '{schemaVersion:1,version:$version,sourceCommit:$commit,typeScriptPackage:$ts,goModuleArchive:$go}' >"$output_dir/secondbox-${version}-package-metadata.json"
jq -n --arg version "$version" --arg commit "$source_commit" '{spdxVersion:"SPDX-2.3",dataLicense:"CC0-1.0",SPDXID:"SPDXRef-DOCUMENT",name:("SecondBox-"+$version),documentNamespace:("https://github.com/SecondStack-AI/SecondBox/releases/tag/v"+$version),creationInfo:{creators:["Organization: SecondStack AI"],comment:("deterministic source commit "+$commit)},packages:[{name:"SecondBox",SPDXID:"SPDXRef-Package-SecondBox",versionInfo:$version,downloadLocation:("git+https://github.com/SecondStack-AI/SecondBox.git@"+$commit),filesAnalyzed:false}]}' >"$output_dir/secondbox-${version}.spdx.json"
chmod 0644 "$output_dir/$openapi_name" "$output_dir/$go_module_name" "$output_dir/$typescript_name" "$output_dir/secondbox-${version}-package-metadata.json" "$output_dir/secondbox-${version}.spdx.json" "$output_dir/install.sh"

go -C "$repo_root" run ./cmd/secondbox-release-tool write-shared-artifacts-record "$output_dir" "$version" "$source_commit" "$release_postgres_image"
echo "$output_dir"
