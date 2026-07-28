#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

go run ./cmd/generate-clients --check
scripts/verify-runner-protocol-generated.sh
scripts/verify-guest-protocol-generated.sh
go test ./sdk/go/secondboxclient
python3 -c 'from pathlib import Path; source = Path("sdk/python/secondbox_client_gen.py").read_text(); compile(source, "sdk/python/secondbox_client_gen.py", "exec")'
PYTHONPATH=sdk/python python3 -c 'import secondbox_client_gen'

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
  'await import("./sdk/typescript/dist/client.js"); await import("./sdk/typescript/dist/flue.js")'
diff -u LICENSE sdk/typescript/LICENSE
npm run pack:sdk-typescript
