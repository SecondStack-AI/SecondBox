#!/usr/bin/env bash
set -euo pipefail

[[ "$(uname -s)" == "Linux" ]] || {
  echo "SecondBox Linux WorkspaceStore qualification requires Linux" >&2
  exit 1
}
: "${SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE:?set this to the pinned local helper executable}"
[[ "$SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE" = /* && -x "$SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE" ]] || {
  echo "SecondBox Linux WorkspaceStore qualification requires an absolute executable helper path" >&2
  exit 1
}

qualification_parent="${SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM:-$PWD}"
[[ "$qualification_parent" = /* && -d "$qualification_parent" ]] || {
  echo "SecondBox Linux WorkspaceStore qualification filesystem must be an absolute directory" >&2
  exit 1
}

export SECONDBOX_REQUIRE_WORKSPACESTORE_LINUX=1
export SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM="$qualification_parent"
export GOCACHE="${GOCACHE:-$(mktemp -d /tmp/secondbox-workspacestore-gocache.XXXXXXXX)}"
cd runner
go test ./internal/workspacestore/... -count=1
