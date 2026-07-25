#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

find agent -type f \
    ! -path 'agent/node_modules/*' \
    ! -path 'agent/dist/*' \
    -print0 \
    | sort -z \
    | xargs -0 sha256sum \
    | sha256sum \
    | cut -d' ' -f1
