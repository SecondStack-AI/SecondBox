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

build-artifacts:
    scripts/build-artifacts.sh

test-firecracker:
    scripts/test-firecracker.sh

test-multirunner:
    scripts/test-multirunner.sh

test-scenario:
    scripts/test-scenario.sh

deploy-bootstrap environment:
    deploy/bin/bootstrap-environment.sh "{{environment}}"

deploy-development-prepare environment:
    deploy/bin/prepare-development-inventory.sh "{{environment}}"

deploy-validate environment:
    deploy/bin/validate-environment.sh "{{environment}}"

deploy-config environment:
    deploy/bin/validate-environment.sh "{{environment}}"
    docker compose --env-file "{{environment}}" --file deploy/compose.yml config --quiet

preship: test-non-kvm
