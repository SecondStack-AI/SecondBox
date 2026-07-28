# SecondBox build and validation tasks.

default:
    @just --list

generate:
    go run ./cmd/generate-clients

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

test-clean-clone:
    scripts/test-clean-clone-isolation.sh --non-kvm

test-operations:
    go test ./tests/operations -count=1

test-backup-restore:
    @test -n "${SECONDBOX_TEST_DATABASE_URL}" || (echo "SECONDBOX_TEST_DATABASE_URL is required" >&2; exit 2)
    go test ./tests/integration -run '^TestBackupRestoreDrillMaterializesCheckpointOnFreshRunner$' -count=1

build-artifacts:
    scripts/build-artifacts.sh

test-firecracker:
    scripts/test-firecracker.sh

test-multirunner:
    scripts/test-multirunner.sh

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
