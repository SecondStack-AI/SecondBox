#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT

typescript_root="$test_root/typescript"
mkdir -p "$typescript_root"
package_json="$(npm pack "$repo_root/sdk/typescript" --silent --json --pack-destination "$typescript_root")"
tarball_name="$(jq -er '.[0].filename' <<<"$package_json")"
(
    cd "$typescript_root"
    npm init --yes >/dev/null
    npm install --ignore-scripts --no-audit --no-fund "./$tarball_name" >/dev/null
    node --input-type=module -e '
      import { SecondBox, SecondBoxClient } from "@secondstack-ai/secondbox";
      import { createSecondBoxFlueAdapter } from "@secondstack-ai/secondbox/flue";
      import { createNodeTransports } from "@secondstack-ai/secondbox/node";
      const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetch));
      if (typeof api.adoptSandbox !== "function" || typeof createSecondBoxFlueAdapter !== "function" || typeof createNodeTransports !== "function") process.exit(1);
    '
)

go_root="$test_root/go"
mkdir -p "$go_root"
(
    cd "$go_root"
    go mod init secondbox-sdk-consumer >/dev/null
    if [[ -n "${SECONDBOX_GO_RELEASE_VERSION:-}" ]]; then
        go get "github.com/SecondStack-AI/SecondBox@${SECONDBOX_GO_RELEASE_VERSION}"
    else
        go mod edit -require=github.com/SecondStack-AI/SecondBox@v0.0.0
        go mod edit -replace="github.com/SecondStack-AI/SecondBox=$repo_root"
    fi
    printf '%s\n' \
        'package consumer' \
        'import secondbox "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"' \
        'var _ = secondbox.PageOptions{Limit: 1}' > consumer_test.go
    go mod tidy
    go test ./...
)

echo "SecondBox SDK clean-project package tests passed"
