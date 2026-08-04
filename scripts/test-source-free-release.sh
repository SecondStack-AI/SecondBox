#!/usr/bin/env bash
set -euo pipefail

required=(
  SECONDBOX_RELEASE_ARTIFACT_MANIFEST_URL
  SECONDBOX_SOURCE_FREE_OPERATOR_MANIFEST
  SECONDBOX_SOURCE_FREE_ROOT
  SECONDBOX_SOURCE_FREE_ARTIFACT_DIRECTORY
  SECONDBOX_SOURCE_FREE_QUALIFICATION_OUTPUT
  SECONDBOX_URL
  SECONDBOX_TOKEN
  SECONDBOX_TENANT_REF
  SECONDBOX_SUBJECT_REF
)
for name in "${required[@]}"; do
  [[ -n "${!name:-}" ]] || { echo "source-free release qualification requires $name" >&2; exit 1; }
done
for command in curl jq sha256sum docker npm node go uname date; do
  command -v "$command" >/dev/null || { echo "source-free release qualification requires $command" >&2; exit 1; }
done

root="$SECONDBOX_SOURCE_FREE_ROOT"
[[ "$root" == /* && ! -e "$root" ]] || { echo "SECONDBOX_SOURCE_FREE_ROOT must be an absent absolute path" >&2; exit 1; }
artifact_directory="$SECONDBOX_SOURCE_FREE_ARTIFACT_DIRECTORY"
[[ "$artifact_directory" == /* && -d "$artifact_directory" && -z "$(find "$artifact_directory" -mindepth 1 -maxdepth 1 -print -quit)" ]] || {
  echo "SECONDBOX_SOURCE_FREE_ARTIFACT_DIRECTORY must be an existing empty absolute directory" >&2
  exit 1
}
mkdir -m 0700 "$root"
manifest="$root/artifact-manifest.json"
curl --fail --location --silent --show-error "$SECONDBOX_RELEASE_ARTIFACT_MANIFEST_URL" >"$manifest"
version="$(jq -er '.version' "$manifest")"
platform="linux_amd64"
deploy_url="$(jq -er --arg platform "linux/amd64" '.binaries[] | select(.name == "secondbox-deploy" and .platform == $platform) | .location' "$manifest")"
deploy_sha="$(jq -er --arg platform "linux/amd64" '.binaries[] | select(.name == "secondbox-deploy" and .platform == $platform) | .sha256' "$manifest")"
deploy="$root/secondbox-deploy"
curl --fail --location --silent --show-error "$deploy_url" >"$deploy"
echo "$deploy_sha  $deploy" | sha256sum --check --strict >/dev/null
chmod 0755 "$deploy"
"$deploy" verify artifact-manifest "$SECONDBOX_RELEASE_ARTIFACT_MANIFEST_URL" >/dev/null

control_image="$(jq -er '.controlPlane.reference' "$manifest")"
runner_image="$(jq -er '.runner.reference' "$manifest")"
microvm_image="$(jq -er '.microvm.imageReference' "$manifest")"
docker pull "$control_image" >/dev/null
docker pull "$runner_image" >/dev/null
docker pull "$microvm_image" >/dev/null
microvm_container="$(docker create --entrypoint /nonexistent "$microvm_image")"
docker cp "$microvm_container:/secondbox-runner-microvm/." "$artifact_directory"
docker rm "$microvm_container" >/dev/null

deployment="$root/deployment"
"$deploy" init --mode production --input "$SECONDBOX_SOURCE_FREE_OPERATOR_MANIFEST" --qualification-artifact-manifest "$SECONDBOX_RELEASE_ARTIFACT_MANIFEST_URL" "$deployment" >/dev/null
"$deploy" compose "$deployment/secondbox.toml" up
first_profiles="$("$deploy" inspect "$deployment/secondbox.toml" | jq -cS '.standardProfiles')"
"$deploy" compose "$deployment/secondbox.toml" up
second_profiles="$("$deploy" inspect "$deployment/secondbox.toml" | jq -cS '.standardProfiles')"
[[ "$first_profiles" == "$second_profiles" ]] || { echo "repeated source-free deployment changed standard Profile identity" >&2; exit 1; }

node_project="$root/typescript-sdk"
mkdir "$node_project"
(
  cd "$node_project"
  npm init --yes >/dev/null
  npm install --ignore-scripts --no-audit --no-fund "@secondstack-ai/secondbox@${version}" >/dev/null
  cat >lifecycle.mjs <<'EOF'
import {SecondBox, SecondBoxClient} from "@secondstack-ai/secondbox";
const api = new SecondBox(new SecondBoxClient(process.env.SECONDBOX_URL, process.env.SECONDBOX_TOKEN, fetch, process.env.SECONDBOX_TENANT_REF, process.env.SECONDBOX_SUBJECT_REF));
await api.validateProfile("durable-coding");
const {handle} = await api.createSandbox({profile:"durable-coding",metadata:{qualification:"typescript"}});
await handle.waitFor(["ready"], {deadlineMilliseconds:600000});
const result = await handle.exec("printf source-free-typescript", {environment:{},deadlineMilliseconds:30000,maximumOutputBytes:1048576});
if (result.kind !== "exited" || new TextDecoder().decode(result.stdout) !== "source-free-typescript") throw new Error("TypeScript lifecycle output mismatch");
await handle.stop({});
EOF
  node lifecycle.mjs
)

go_project="$root/go-sdk"
mkdir "$go_project"
(
  cd "$go_project"
  go mod init secondbox-source-free-qualification >/dev/null
  GONOSUMDB= GOPRIVATE= GOPROXY=https://proxy.golang.org go get "github.com/SecondStack-AI/SecondBox@v${version}"
  cat >main.go <<'EOF'
package main
import("context";"fmt";"net/http";"os";"time"; secondbox "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient")
func main(){c,e:=secondbox.NewSecondBoxSubjectClient(os.Getenv("SECONDBOX_URL"),os.Getenv("SECONDBOX_TOKEN"),os.Getenv("SECONDBOX_TENANT_REF"),os.Getenv("SECONDBOX_SUBJECT_REF"),http.DefaultClient);if e!=nil{panic(e)};ctx,cancel:=context.WithTimeout(context.Background(),10*time.Minute);defer cancel();if _,e=c.ValidateProfile(ctx,"durable-coding");e!=nil{panic(e)};h,_,e:=c.CreateSandbox(ctx,secondbox.CreateSandboxRequest{Profile:"durable-coding",Metadata:secondbox.Metadata{"qualification":"go"}},"");if e!=nil{panic(e)};if _,e=h.WaitFor(ctx,secondbox.SandboxStateReady);e!=nil{panic(e)};o,e:=h.Execute(ctx,secondbox.BufferedExecRequest{Command:secondbox.Command{ShellCommand:&secondbox.ShellCommand{Mode:"shell",Command:"printf source-free-go"}},Environment:secondbox.StringMap{},DeadlineMilliseconds:30000,MaximumOutputBytes:1048576},"","");if e!=nil{panic(e)};r,e:=secondbox.DecodeExecOutcome(o);if e!=nil{panic(e)};if string(r.Stdout)!="source-free-go"{panic(fmt.Sprintf("output %q",r.Stdout))};if _,e=h.Stop(ctx,secondbox.LifecycleOptions{});e!=nil{panic(e)}}
EOF
  go run .
)

compose="$root/compose.json"
"$deploy" inspect "$deployment/secondbox.toml" >"$compose"
jq -e --arg control "$control_image" --arg runner "$runner_image" '.environment.SECONDBOX_CONTROL_PLANE_IMAGE == $control and .environment.SECONDBOX_RUNNER_IMAGE == $runner' "$compose" >/dev/null
if find "$deployment" -type d -name .git -o -type f \( -name go.mod -o -name package.json -o -name Dockerfile \) | grep -q .; then
  echo "installed deployment contains a source/build dependency" >&2
  exit 1
fi

qualification_input="$root/qualification-input.json"
jq -n \
  --arg suite "sha256:$(sha256sum "$0" | awk '{print $1}')" \
  --arg kernel "$(uname -srmo)" \
  --arg firecracker "$(docker run --rm --entrypoint /usr/local/bin/firecracker "$runner_image" --version | head -1)" \
  --arg cpu "$(awk -F: '/model name/{gsub(/^ +/,"",$2); print $2; exit}' /proc/cpuinfo)" \
  --arg completed "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{suiteDigest:$suite,operatingSystem:"linux",kernel:$kernel,firecrackerVersion:$firecracker,cpuModel:$cpu,completedAt:$completed}' >"$qualification_input"
"$deploy" qualification-attestation --manifest "$manifest" --input "$qualification_input" --output "$SECONDBOX_SOURCE_FREE_QUALIFICATION_OUTPUT"
echo "SecondBox source-free release qualification passed for v${version}"
