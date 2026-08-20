# SecondBox build and validation tasks.

default:
    @just --list

verify-generated:
    scripts/verify-generated.sh

build:
    go build -o /dev/null ./cmd/secondboxd

build-microsandbox-probe-linux source output:
    just -f runner/Justfile build-microsandbox-probe-linux "{{source}}" "{{output}}"

build-microsandbox-probe-macos source output:
    just -f runner/Justfile build-microsandbox-probe-macos "{{source}}" "{{output}}"

test-microsandbox-probe-macos build_dir work_dir:
    just -f runner/Justfile test-microsandbox-probe-macos "{{build_dir}}" "{{work_dir}}"

test-microsandbox-probe-ext4-linux build_dir work_dir:
    just -f runner/Justfile test-microsandbox-probe-ext4-linux "{{build_dir}}" "{{work_dir}}"

test-microsandbox-probe-linux build_dir work_dir:
    just -f runner/Justfile test-microsandbox-probe-linux "{{build_dir}}" "{{work_dir}}"

lint:
    golangci-lint run ./...
    cd runner && golangci-lint run --config ../.golangci.yml ./...

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

test-workspacestore-linux:
    scripts/test-workspacestore-linux.sh

test-workspacestore-macos:
    scripts/test-workspacestore-macos.sh

test-microsandbox-linux:
    scripts/test-microsandbox-linux.sh

test-microsandbox-macos:
    scripts/test-microsandbox-macos.sh

test-deployment:
    go test ./tests/deployment -count=1

test-sdk-packages:
    scripts/test-sdk-packages.sh

test-cli-ui:
    go test ./internal/cliui ./cmd/secondbox ./cmd/secondbox-deploy ./tests/cliui -count=1
    go test -race ./internal/cliui ./cmd/secondbox -run 'Activity|Progress|Stream|Shell|Exec|Run' -count=1

test-install-docs:
    scripts/test-install-docs.sh

test-installer:
    go test ./internal/install ./internal/deployconfig ./pkg/releasecontract ./pkg/releaseverify ./cmd/secondbox-deploy ./tests/cliui -count=1
    scripts/test-install-docs.sh
    scripts/test-installer-unattended.sh

test-installer-vm:
    scripts/test-installer-vm.sh

test-installer-qualified:
    scripts/test-installer-qualified.sh

test-standard-resources:
    scripts/test-standard-resources.sh

build-artifacts:
    scripts/build-artifacts.sh

release-stage version output_dir:
    scripts/release-stage.sh "{{version}}" "{{output_dir}}"

release-candidate version output_dir:
    scripts/release-stage.sh --candidate "{{version}}" "{{output_dir}}"

test-release-stage:
    scripts/test-release-stage.sh

test-release-workflow:
    scripts/test-release-workflow.sh

release-upload version output_dir:
    scripts/release-upload.sh "{{version}}" "{{output_dir}}"

test-firecracker:
    scripts/test-firecracker.sh

test-snapshot-resume:
    scripts/test-snapshot-resume.sh

test-snapshot-resume-jailed:
    scripts/test-snapshot-resume-jailed.sh

test-multirunner:
    scripts/test-multirunner.sh

test-scenario:
    scripts/test-scenario.sh

test-scenario-microsandbox-linux:
    scripts/test-scenario-microsandbox-linux.sh

test-scenario-microsandbox-macos:
    scripts/test-scenario-microsandbox-macos.sh

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
