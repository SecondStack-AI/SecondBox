# SecondBox build and validation tasks.

default:
    @just --list

verify-generated:
    scripts/verify-generated.sh

build:
    go build -o /dev/null ./cmd/secondboxd

test:
    scripts/test-go.sh

test-contract:
    scripts/test-contract.sh

test-compose:
    scripts/test-compose.sh

test-image-policy:
    scripts/test-image-policy.sh

test-non-kvm:
    scripts/test-non-kvm.sh

test-deployment:
    go test ./tests/deployment -count=1

test-sdk-packages:
    scripts/test-sdk-packages.sh

test-standard-resources:
    scripts/test-standard-resources.sh

build-artifacts:
    scripts/build-artifacts.sh

release-stage version output_dir:
    scripts/release-stage.sh "{{version}}" "{{output_dir}}"

test-release-stage:
    scripts/test-release-stage.sh

test-release-workflow:
    scripts/test-release-workflow.sh

release-local-prepare version output_dir:
    scripts/release-local-prepare.sh "{{version}}" "{{output_dir}}"

release-local-upload version prepared_dir:
    scripts/release-local-upload.sh "{{version}}" "{{prepared_dir}}"

release-local-qualify version operator_manifest work_dir:
    scripts/release-local-qualify.sh "{{version}}" "{{operator_manifest}}" "{{work_dir}}"

release-local-finalize version qualification_file:
    scripts/release-local-finalize.sh "{{version}}" "{{qualification_file}}"

test-source-free-release:
    scripts/test-source-free-release.sh

test-firecracker:
    scripts/test-firecracker.sh

test-snapshot-resume:
    scripts/test-snapshot-resume.sh

test-multirunner:
    scripts/test-multirunner.sh

test-scenario:
    scripts/test-scenario.sh

prepare-stress:
    scripts/prepare-stress.sh

test-stress:
    scripts/test-stress.sh

test-lifecycle:
    scripts/test-lifecycle.sh

deploy-init-development directory:
    go run ./cmd/secondbox-deploy init --mode development "{{directory}}"

deploy-init-production directory:
    go run ./cmd/secondbox-deploy init --mode production "{{directory}}"

deploy-validate manifest:
    go run ./cmd/secondbox-deploy validate "{{manifest}}"

deploy-config manifest:
    go run ./cmd/secondbox-deploy compose "{{manifest}}" config

deploy-development-prepare manifest:
    go run ./cmd/secondbox-deploy compose "{{manifest}}" prepare

deploy-up manifest:
    go run ./cmd/secondbox-deploy compose "{{manifest}}" up

deploy-down manifest:
    go run ./cmd/secondbox-deploy compose "{{manifest}}" down

deploy-development-up directory:
    #!/usr/bin/env bash
    set -euo pipefail
    manifest="{{directory}}/secondbox.toml"
    if [[ ! -e "$manifest" ]]; then
      if [[ -e "{{directory}}" ]]; then
        echo "SecondBox deployment directory exists without secondbox.toml; refusing to replace it" >&2
        exit 1
      fi
      go run ./cmd/secondbox-deploy init --mode development "{{directory}}"
    fi
    docker build --tag secondbox-control-plane:development .
    go run ./cmd/secondbox-deploy compose "$manifest" config
    go run ./cmd/secondbox-deploy compose "$manifest" prepare
    go run ./cmd/secondbox-deploy compose "$manifest" up
    inspection="$(go run ./cmd/secondbox-deploy inspect "$manifest")"
    public_base_url="$(jq -er '.environment.SECONDBOX_PUBLIC_BASE_URL' <<<"$inspection")"
    wait_seconds="$(jq -er '.developmentWaitSeconds' <<<"$inspection")"
    curl --fail --silent --show-error --retry "$wait_seconds" --retry-all-errors --retry-delay 1 "${public_base_url%/}/readyz" >/dev/null
    echo "SecondBox development control plane is ready at $public_base_url"

preship: test-non-kvm
