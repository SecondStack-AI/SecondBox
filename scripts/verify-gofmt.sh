#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Check tracked Go source in both modules, including generated and build-tagged files.
unformatted="$(git ls-files -z '*.go' | xargs -0 gofmt -l)"
if [[ -n "$unformatted" ]]; then
    printf 'SecondBox Go files require gofmt:\n%s\n' "$unformatted" >&2
    echo "Run: git ls-files -z '*.go' | xargs -0 gofmt -w" >&2
    exit 1
fi
