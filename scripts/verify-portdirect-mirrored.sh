#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# The control plane and the privileged Runner each own a copy of the direct data-plane
# handshake, for the same reason the generated protocol code is duplicated: the
# two modules deliberately share no dependency graph. The copies must stay
# byte-identical below the package declaration, whose doc comment names the
# opposite side and is therefore expected to differ.
control_plane="pkg/portdirect/framing.go"
runner="runner/internal/portdirect/framing.go"

body() {
    sed -n '/^package portdirect$/,$p' "$1"
}

for source in "$control_plane" "$runner"; do
    if ! grep -q '^package portdirect$' "$source"; then
        echo "SecondBox direct data-plane handshake copy has no package declaration: $source" >&2
        exit 1
    fi
done

diff -u <(body "$control_plane") <(body "$runner") || {
    echo "SecondBox direct data-plane handshake copies have diverged: $control_plane vs $runner" >&2
    exit 1
}
