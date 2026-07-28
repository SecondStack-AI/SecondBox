#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode=build
case "${1-}" in
  "")
    ;;
  --scan-only)
    mode=scan
    shift
    ;;
  --non-kvm)
    mode=non-kvm
    shift
    ;;
  *)
    echo "Usage: scripts/test-clean-clone-isolation.sh [--scan-only|--non-kvm]" >&2
    exit 2
    ;;
esac
if [[ "$#" -ne 0 ]]; then
  echo "Usage: scripts/test-clean-clone-isolation.sh [--scan-only|--non-kvm]" >&2
  exit 2
fi

for required_command in find git go realpath rg; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox source isolation scan requires command: $required_command" >&2
    exit 1
  fi
done

if [[ -n "$(git -C "$repo_root" submodule status)" ]]; then
  echo "SecondBox clean-clone isolation forbids git submodules" >&2
  exit 1
fi
if rg -n '^[[:space:]]*replace[[:space:]]+.*=>[[:space:]]+(\\.\\.?/|/)' \
  "$repo_root/go.mod" "$repo_root/runner/go.mod"; then
  echo "SecondBox clean-clone isolation forbids local Go module replacements" >&2
  exit 1
fi
if rg -n \
  --hidden \
  --glob '!.git/**' \
  --glob '*.go' \
  --glob '*.sh' \
  --glob '*.yml' \
  --glob '*.yaml' \
  --glob '*.js' \
  --glob '*.mjs' \
  --glob '*.py' \
  --glob '*.service' \
  --glob '*.ts' \
  --glob '*.toml' \
  --glob 'Dockerfile*' \
  --glob 'Justfile' \
  --glob 'Makefile' \
  --glob '*.example' \
  --glob '*.mk' \
  --glob 'go.mod' \
  --glob 'package-lock.json' \
  --glob 'package.json' \
  --glob '!test-clean-clone-isolation.sh' \
  '(\\.\\./SecondStack|/SecondStack/|SecondStack-AI/SecondStack|apps/sandbox-service)' \
  "$repo_root"; then
  echo "SecondBox clean-clone isolation found a SecondStack filesystem reach-through" >&2
  exit 1
fi

while IFS= read -r symbolic_link; do
  resolved_path="$(realpath "$symbolic_link")"
  case "$resolved_path" in
    "$repo_root"/*) ;;
    *)
      echo "SecondBox clean-clone isolation found an escaping symbolic link: $symbolic_link -> $resolved_path" >&2
      exit 1
      ;;
  esac
done < <(
  find "$repo_root" \
    -path "$repo_root/.git" -prune -o \
    -path "$repo_root/.tmp" -prune -o \
    -path "$repo_root/dist" -prune -o \
    -path "$repo_root/node_modules" -prune -o \
    -type l -print
)

GOWORK=off go -C "$repo_root" list ./... >/dev/null
GOWORK=off go -C "$repo_root/runner" list ./... >/dev/null

if [[ "$mode" == scan ]]; then
  echo "SecondBox source isolation scan passed"
  exit 0
fi

if ! command -v npm >/dev/null 2>&1; then
  echo "SecondBox clean-clone isolation requires command: npm" >&2
  exit 1
fi

if [[ -n "$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)" ]]; then
  echo "SecondBox clean-clone isolation requires a clean source tree so the clone includes the exact candidate" >&2
  exit 1
fi

clean_clone_parent="$(mktemp -d)"
clean_clone_path="$clean_clone_parent/SecondBox"
source_commit="$(git -C "$repo_root" rev-parse HEAD)"
cleanup_clean_clone() {
  if [[ -n "${clean_module_cache-}" ]] &&
      [[ -d "$clean_module_cache" ]] &&
      ! chmod -R u+w "$clean_module_cache"; then
    echo "SecondBox clean-clone isolation failed to make its temporary Go module cache removable: $clean_module_cache" >&2
    return 1
  fi
  if [[ -d "$clean_clone_parent" ]] && ! rm -rf -- "$clean_clone_parent"; then
    echo "SecondBox clean-clone isolation failed to remove temporary directory: $clean_clone_parent" >&2
    return 1
  fi
}
trap cleanup_clean_clone EXIT
git clone --quiet --no-hardlinks --no-local "$repo_root" "$clean_clone_path"

clean_module_cache="$clean_clone_parent/go-mod-cache"
clean_build_cache="$clean_clone_parent/go-build-cache"
clean_npm_cache="$clean_clone_parent/npm-cache"
(
  export GOWORK=off
  export GOMODCACHE="$clean_module_cache"
  export GOCACHE="$clean_build_cache"
  export NPM_CONFIG_CACHE="$clean_npm_cache"
  cd "$clean_clone_path"
  if [[ "$mode" == non-kvm ]]; then
    scripts/test-non-kvm.sh
  else
    npm ci --ignore-scripts
    scripts/verify-generated.sh
    scripts/build-artifacts.sh
  fi
)
clean_clone_commit="$(git -C "$clean_clone_path" rev-parse HEAD)"
if [[ "$clean_clone_commit" != "$source_commit" ]]; then
  echo "SecondBox clean-clone isolation tested commit $clean_clone_commit instead of source commit $source_commit" >&2
  exit 1
fi
if [[ -n "$(git -C "$clean_clone_path" status --porcelain=v1 --untracked-files=all)" ]]; then
  echo "SecondBox clean-clone isolation matrix modified tracked or unignored source files" >&2
  git -C "$clean_clone_path" status --short >&2
  exit 1
fi
cleanup_clean_clone
trap - EXIT
if [[ "$mode" == non-kvm ]]; then
  echo "SecondBox clean-clone non-KVM qualification passed from commit $clean_clone_commit"
else
  echo "SecondBox clean-clone isolation passed from commit $clean_clone_commit"
fi
