#!/usr/bin/env bash
set -euo pipefail

# Prints the source commit a release build binds. Test mode binds a digest of
# the working tree instead, so release staging tests run on uncommitted changes.
export LC_ALL=C
[[ "$#" -le 1 && "${1:---test-mode}" == --test-mode ]] || { echo "usage: scripts/release-source-commit.sh [--test-mode]" >&2; exit 2; }
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "$#" -eq 0 ]]; then
  git -C "$repo_root" rev-parse HEAD
  exit 0
fi
while IFS= read -r file; do
  [[ -f "$repo_root/$file" ]] || continue
  printf '%s %s\n' "$file" "$(sha256sum "$repo_root/$file" | awk '{print $1}')"
done < <(git -C "$repo_root" ls-files --cached --others --exclude-standard | sort) |
  sha256sum | awk '{print substr($1,1,40)}'
