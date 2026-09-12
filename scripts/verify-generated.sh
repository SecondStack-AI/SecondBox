#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Keep the compiler identical to the reviewed generator pin.
protoc_version="$(sed -n 's/^version="\([^"]*\)"$/\1/p' scripts/install-protoc.sh)"
if [[ "$(protoc --version 2>/dev/null || true)" != "libprotoc $protoc_version" ]]; then
  mkdir -p .tmp
  (
    flock 9
    if [[ "$(.tmp/protoc/bin/protoc --version 2>/dev/null || true)" != "libprotoc $protoc_version" ]]; then
      temporary="$(mktemp -d .tmp/protoc-install.XXXXXX)"
      trap 'rm -rf -- "$temporary"' EXIT
      scripts/install-protoc.sh "$temporary"
      rm -rf -- .tmp/protoc
      mv -- "$temporary" .tmp/protoc
    fi
  ) 9>.tmp/protoc.lock
  export PATH="$PWD/.tmp/protoc/bin:$PATH"
fi

scripts/verify-runner-protocol-generated.sh
scripts/verify-guest-protocol-generated.sh
scripts/verify-microsandbox-helper-generated.sh
scripts/verify-portdirect-mirrored.sh
scripts/verify-network-policy-contract-mirrored.sh
cmp pkg/egressattribution/framing.go runner/egressattribution/framing.go
cmp pkg/egressattribution/peer_linux.go runner/egressattribution/peer_linux.go
scripts/verify-sdk-generated.sh
go test ./internal/deployconfig -run TestExampleManifestIsGeneratedFromTheRegistry -count=1
go test ./sdk/go/secondboxclient

if [[ ! -x node_modules/.bin/tsc ]]; then
  echo "SecondBox TypeScript validation requires npm ci --ignore-scripts" >&2
  exit 1
fi

npm run typecheck:sdk-typescript
npm run test:sdk-typescript
npm run build:sdk-typescript
if rg -n 'from "[.]/.*[.]ts"' sdk/typescript/dist; then
  echo "SecondBox TypeScript SDK declarations retain source-only .ts specifiers" >&2
  exit 1
fi
node --input-type=module --eval \
  'await import("./sdk/typescript/dist/transport.js"); await import("./sdk/typescript/dist/client.js"); await import("./sdk/typescript/dist/flue.js")'
diff -u LICENSE sdk/typescript/LICENSE
npm run pack:sdk-typescript
