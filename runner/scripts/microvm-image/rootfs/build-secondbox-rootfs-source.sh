#!/usr/bin/env bash
set -euo pipefail

# Build a prepared SecondBox guest rootfs from exactly one explicit, immutable
# source: a content-addressed OCI image or a declarative Debian image definition.

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../../../.." && pwd)"

fail_secondbox_image_build() {
    echo "SecondBox image pipeline failed: $*" >&2
    exit 2
}

require_secondbox_build_command() {
    command -v "$1" >/dev/null 2>&1 ||
        fail_secondbox_image_build "missing required command: $1"
}

run_secondbox_build_as_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
        return
    fi
    require_secondbox_build_command sudo
    sudo "$@"
}

sha256_secondbox_build_file() {
    sha256sum "$1" | awk '{print $1}'
}

validate_secondbox_oci_reference() {
    local reference="$1"
    [[ "$reference" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] ||
        fail_secondbox_image_build \
            "SECONDBOX_RUNNER_MICROVM_OCI_BASE_REFERENCE must end with @sha256:<64 lowercase hex characters>"
}

load_secondbox_image_definition() {
    local definition_file="$1"
    local definition_record
    require_secondbox_build_command python3
    require_secondbox_build_command sha256sum
    [ -f "$definition_file" ] ||
        fail_secondbox_image_build "image definition not found: $definition_file"

    definition_record="$(
        python3 - "$definition_file" <<'PY'
import json
import re
import sys
from pathlib import Path

path = Path(sys.argv[1])
definition = json.loads(path.read_text(encoding="utf-8"))
if definition.get("schemaVersion") != 1:
    raise SystemExit("SecondBox image definition rejected: schemaVersion must be 1")
if definition.get("kind") != "secondbox.microvm-rootfs":
    raise SystemExit("SecondBox image definition rejected: kind must be secondbox.microvm-rootfs")

guest_protocol = definition.get("guestProtocol")
if not isinstance(guest_protocol, dict):
    raise SystemExit("SecondBox image definition rejected: guestProtocol is required")
guest_minimum = guest_protocol.get("minimum")
guest_maximum = guest_protocol.get("maximum")
if (
    not isinstance(guest_minimum, int)
    or not isinstance(guest_maximum, int)
    or guest_minimum < 1
    or guest_minimum > guest_maximum
):
    raise SystemExit("SecondBox image definition rejected: invalid guestProtocol range")

debian = definition.get("debian")
if not isinstance(debian, dict):
    raise SystemExit("SecondBox image definition rejected: debian is required")
suite = debian.get("suite")
architecture = debian.get("architecture")
snapshot = debian.get("snapshot")
apt_check_valid_until = debian.get("aptCheckValidUntil")
include = debian.get("debootstrapInclude")
if not isinstance(suite, str) or not re.fullmatch(r"[a-z0-9][a-z0-9.-]*", suite):
    raise SystemExit("SecondBox image definition rejected: invalid Debian suite")
if not isinstance(architecture, str) or not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", architecture):
    raise SystemExit("SecondBox image definition rejected: invalid Debian architecture")
if not isinstance(snapshot, str) or not re.fullmatch(
    r"https://snapshot\.debian\.org/archive/debian/[0-9]{8}T[0-9]{6}Z/", snapshot
):
    raise SystemExit("SecondBox image definition rejected: Debian snapshot must be dated and immutable")
if not isinstance(apt_check_valid_until, bool):
    raise SystemExit("SecondBox image definition rejected: aptCheckValidUntil must be boolean")
if (
    not isinstance(include, list)
    or not include
    or any(not isinstance(package, str) or not re.fullmatch(r"[a-z0-9][a-z0-9+.-]*", package) for package in include)
):
    raise SystemExit("SecondBox image definition rejected: invalid debootstrapInclude")

values = [
    suite,
    architecture,
    snapshot,
    "true" if apt_check_valid_until else "false",
    ",".join(sorted(include)),
    str(guest_minimum),
    str(guest_maximum),
]
print("\t".join(values))
PY
    )"
    IFS=$'\t' read -r debian_suite debian_arch debian_snapshot apt_check_valid_until \
        debootstrap_include guest_protocol_minimum guest_protocol_maximum <<<"$definition_record"
    image_definition_sha256="$(sha256_secondbox_build_file "$definition_file")"
}

write_secondbox_source_manifest() {
    local provenance_dir="$out_dir/usr/share/secondbox/image-provenance"
    local source_reference=""
    local source_digest=""
    local definition_name=""

    if [ "$source_kind" = "oci" ]; then
        source_reference="$oci_base_reference"
        source_digest="${oci_base_reference##*@}"
    else
        definition_name="$(basename "$image_definition")"
        [ "$(sha256_secondbox_build_file "$image_definition")" = "$image_definition_sha256" ] ||
            fail_secondbox_image_build "image definition changed during the build"
    fi
    [ "$(sha256_secondbox_build_file "$script_dir/secondbox-apt-packages.txt")" = "$apt_packages_sha256" ] ||
        fail_secondbox_image_build "Debian package input changed during the build"
    [ "$(sha256_secondbox_build_file "$script_dir/secondbox-python-requirements.txt")" = "$python_requirements_sha256" ] ||
        fail_secondbox_image_build "Python requirement input changed during the build"
    [ "$(sha256_secondbox_build_file "$script_dir/Dockerfile")" = "$dockerfile_sha256" ] ||
        fail_secondbox_image_build "Dockerfile changed during the build"

    install -d -m 0755 "$provenance_dir"
    SOURCE_KIND="$source_kind" \
    SOURCE_REFERENCE="$source_reference" \
    SOURCE_DIGEST="$source_digest" \
    DEFINITION_NAME="$definition_name" \
    DEFINITION_SHA256="$image_definition_sha256" \
    BASE_IMAGE_ID="$base_image_id" \
    DEBIAN_SUITE="$debian_suite" \
    DEBIAN_ARCHITECTURE="$debian_arch" \
    DEBIAN_SNAPSHOT="$debian_snapshot" \
    GUEST_PROTOCOL_MINIMUM="$guest_protocol_minimum" \
    GUEST_PROTOCOL_MAXIMUM="$guest_protocol_maximum" \
    APT_PACKAGES_SHA256="$apt_packages_sha256" \
    PYTHON_REQUIREMENTS_SHA256="$python_requirements_sha256" \
    DOCKERFILE_SHA256="$dockerfile_sha256" \
    GIT_COMMIT="$(git -C "$repo_root" rev-parse HEAD)" \
    python3 - "$provenance_dir/rootfs-source-manifest.json" <<'PY'
import json
import os
import sys
from pathlib import Path

manifest = {
    "schemaVersion": 1,
    "source": {
        "kind": os.environ["SOURCE_KIND"],
        "ociReference": os.environ["SOURCE_REFERENCE"],
        "ociDigest": os.environ["SOURCE_DIGEST"],
        "imageDefinition": os.environ["DEFINITION_NAME"],
        "imageDefinitionSha256": os.environ["DEFINITION_SHA256"],
        "baseImageId": os.environ["BASE_IMAGE_ID"],
        "debianSuite": os.environ["DEBIAN_SUITE"],
        "debianArchitecture": os.environ["DEBIAN_ARCHITECTURE"],
        "debianSnapshot": os.environ["DEBIAN_SNAPSHOT"],
        "guestProtocolMinimum": os.environ["GUEST_PROTOCOL_MINIMUM"],
        "guestProtocolMaximum": os.environ["GUEST_PROTOCOL_MAXIMUM"],
    },
    "inputs": {
        "aptPackagesSha256": os.environ["APT_PACKAGES_SHA256"],
        "pythonRequirementsSha256": os.environ["PYTHON_REQUIREMENTS_SHA256"],
        "dockerfileSha256": os.environ["DOCKERFILE_SHA256"],
        "gitCommit": os.environ["GIT_COMMIT"],
    },
    "outputs": {
        "debianPackages": "rootfs-debian-packages.lock",
        "pythonFreeze": "rootfs-python.freeze",
        "debianLicenseInventory": "rootfs-debian-license-inventory.json",
        "pythonLicenseInventory": "rootfs-python-license-inventory.json",
    },
}
Path(sys.argv[1]).write_text(
    json.dumps(manifest, indent=2, sort_keys=True) + "\n",
    encoding="utf-8",
)
PY
}

oci_base_reference="${SECONDBOX_RUNNER_MICROVM_OCI_BASE_REFERENCE-}"
image_definition="${SECONDBOX_RUNNER_MICROVM_IMAGE_DEFINITION-}"
out_dir="${SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR-}"
debian_suite=""
debian_arch=""
debian_snapshot=""
guest_protocol_minimum=""
guest_protocol_maximum=""
image_definition_sha256=""

[ -n "$out_dir" ] ||
    fail_secondbox_image_build "SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR is required"
case "$out_dir" in
    /|"$repo_root")
        fail_secondbox_image_build "SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR is too broad: $out_dir"
        ;;
esac
[ ! -e "$out_dir" ] ||
    fail_secondbox_image_build "output path already exists: $out_dir"

if [ -n "$oci_base_reference" ] && [ -n "$image_definition" ]; then
    fail_secondbox_image_build \
        "set exactly one of SECONDBOX_RUNNER_MICROVM_OCI_BASE_REFERENCE or SECONDBOX_RUNNER_MICROVM_IMAGE_DEFINITION"
elif [ -n "$oci_base_reference" ]; then
    source_kind="oci"
    validate_secondbox_oci_reference "$oci_base_reference"
    apt_check_valid_until="true"
elif [ -n "$image_definition" ]; then
    source_kind="secondbox_image_definition"
    load_secondbox_image_definition "$image_definition"
else
    fail_secondbox_image_build \
        "an immutable OCI base reference or explicit SecondBox image definition is required"
fi

case "${1-}" in
    --validate-inputs-only)
        [ "$#" -eq 1 ] || fail_secondbox_image_build "unexpected arguments"
        echo "SecondBox image pipeline inputs valid: $source_kind"
        exit 0
        ;;
    "")
        [ "$#" -eq 0 ] || fail_secondbox_image_build "unexpected arguments"
        ;;
    *)
        fail_secondbox_image_build "unknown argument: $1"
        ;;
esac

require_secondbox_build_command docker
require_secondbox_build_command tar
require_secondbox_build_command sha256sum
require_secondbox_build_command git
apt_packages_sha256="$(sha256_secondbox_build_file "$script_dir/secondbox-apt-packages.txt")"
python_requirements_sha256="$(sha256_secondbox_build_file "$script_dir/secondbox-python-requirements.txt")"
dockerfile_sha256="$(sha256_secondbox_build_file "$script_dir/Dockerfile")"

stage_dir=""
container_id=""
built_image_id_file=""
cleanup_secondbox_image_build() {
    if [ -n "$container_id" ]; then
        docker rm -f "$container_id" >/dev/null
    fi
    if [ -n "$stage_dir" ] && [ -d "$stage_dir" ]; then
        run_secondbox_build_as_root rm -rf "$stage_dir"
    fi
    if [ -n "$built_image_id_file" ] && [ -f "$built_image_id_file" ]; then
        rm -f "$built_image_id_file"
    fi
}
trap cleanup_secondbox_image_build EXIT

if [ "$source_kind" = "oci" ]; then
    echo "[1/4] Resolving immutable OCI base $oci_base_reference" >&2
    docker pull "$oci_base_reference"
    base_image_id="$(docker image inspect --format '{{.Id}}' "$oci_base_reference")"
    base_image_reference="$oci_base_reference"
else
    require_secondbox_build_command debootstrap
    stage_dir="$(mktemp -d)"
    echo "[1/4] Creating Debian $debian_suite rootfs from $debian_snapshot" >&2
    run_secondbox_build_as_root debootstrap \
        --arch="$debian_arch" \
        --variant=minbase \
        --include="$debootstrap_include" \
        "$debian_suite" \
        "$stage_dir" \
        "$debian_snapshot"
    if [ "$apt_check_valid_until" = "false" ]; then
        run_secondbox_build_as_root install -d -m 0755 "$stage_dir/etc/apt/apt.conf.d"
        printf 'Acquire::Check-Valid-Until "false";\n' |
            run_secondbox_build_as_root tee \
                "$stage_dir/etc/apt/apt.conf.d/99secondbox-snapshot" >/dev/null
    fi
    base_image_id="$(
        run_secondbox_build_as_root tar \
            -C "$stage_dir" --owner=0 --group=0 --numeric-owner -cf - . |
            docker import -
    )"
    base_image_reference="$base_image_id"
    run_secondbox_build_as_root rm -rf "$stage_dir"
    stage_dir=""
fi

echo "[2/4] Building SecondBox guest rootfs from $base_image_reference ($base_image_id)" >&2
built_image_id_file="$(mktemp)"
docker build \
    --build-arg "BASE_IMAGE=$base_image_reference" \
    --build-arg "APT_CHECK_VALID_UNTIL=$apt_check_valid_until" \
    --iidfile "$built_image_id_file" \
    "$script_dir"
built_image_id="$(<"$built_image_id_file")"
rm -f "$built_image_id_file"
built_image_id_file=""

echo "[3/4] Exporting prepared rootfs to $out_dir" >&2
mkdir -p "$out_dir"
container_id="$(docker create "$built_image_id" /bin/true)"
docker export "$container_id" |
    tar -C "$out_dir" --numeric-owner \
        --exclude='dev/*' \
        --exclude='proc/*' \
        --exclude='sys/*' \
        -xf -
docker rm -f "$container_id" >/dev/null
container_id=""

echo "[4/4] Recording immutable source and dependency provenance" >&2
write_secondbox_source_manifest

echo "Prepared SecondBox rootfs source: $out_dir" >&2
echo "Provenance: $out_dir/usr/share/secondbox/image-provenance" >&2
