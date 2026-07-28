#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

for cmd in file mkfs.ext4 openssl sha256sum truncate; do
	command -v "$cmd" >/dev/null 2>&1 || { echo "missing required command: $cmd" >&2; exit 2; }
done

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work_dir/private.pem" >/dev/null 2>&1
openssl pkey -in "$work_dir/private.pem" -pubout -out "$work_dir/public.pem" >/dev/null 2>&1
public_key_fingerprint="$(openssl pkey -pubin -in "$work_dir/public.pem" -outform DER | sha256sum | awk '{print $1}')"

make_root() {
	root="$1"
	rm -rf "$root"
	mkdir -p "$root/var/lib/dpkg" "$root/usr/local/bin" "$root/builtin-skills"
	printf 'Package: base-files\nStatus: install ok installed\n' > "$root/var/lib/dpkg/status"
}

make_image() {
	root="$1"
	image="$2"
	truncate -s 32M "$image"
	mkfs.ext4 -F -q -d "$root" "$image"
}

clean_root="$work_dir/clean-root"
make_root "$clean_root"
"$script_dir/verify-browser-surface.sh" --root-dir "$clean_root"

missing_inventory_root="$work_dir/missing-inventory-root"
mkdir -p "$missing_inventory_root/usr/local/bin"
if "$script_dir/verify-browser-surface.sh" --root-dir "$missing_inventory_root" >/dev/null 2>&1; then
	echo "expected missing package inventory rejection" >&2
	exit 1
fi

bad_path_root="$work_dir/bad-path-root"
make_root "$bad_path_root"
printf '#!/bin/sh\nexit 0\n' > "$bad_path_root/usr/local/bin/browser"
chmod 0755 "$bad_path_root/usr/local/bin/browser"
if "$script_dir/verify-browser-surface.sh" --root-dir "$bad_path_root" >/dev/null 2>&1; then
	echo "expected prepared-rootfs browser launcher rejection" >&2
	exit 1
fi

bad_package_root="$work_dir/bad-package-root"
make_root "$bad_package_root"
printf '\nPackage: chromium\nStatus: install ok installed\n' >> "$bad_package_root/var/lib/dpkg/status"
if "$script_dir/verify-browser-surface.sh" --root-dir "$bad_package_root" >/dev/null 2>&1; then
	echo "expected prepared-rootfs Chromium package rejection" >&2
	exit 1
fi

bad_playwright_root="$work_dir/bad-playwright-root"
make_root "$bad_playwright_root"
mkdir -p "$bad_playwright_root/root/.cache/ms-playwright"
if "$script_dir/verify-browser-surface.sh" --root-dir "$bad_playwright_root" >/dev/null 2>&1; then
	echo "expected Playwright browser cache rejection" >&2
	exit 1
fi

clean_image="$work_dir/clean.ext4"
make_image "$clean_root" "$clean_image"
"$script_dir/verify-browser-surface.sh" --rootfs "$clean_image"
"$script_dir/verify-browser-surface.sh" --shared "$clean_image"

make_artifact_dir() {
	artifact_dir="$1"
	rootfs_image="$2"
	shared_image="$3"
	rm -rf "$artifact_dir"
	mkdir -p "$artifact_dir"
	cp "$rootfs_image" "$artifact_dir/rootfs.ext4"
	cp "$shared_image" "$artifact_dir/shared.img"
	printf 'kernel\n' > "$artifact_dir/kernel"
	printf '{"schemaVersion":1}\n' > "$artifact_dir/rootfs-source-manifest.json"
	printf '{"state":"verified"}\n' > "$artifact_dir/secondbox-rootfs-contract.json"
	printf 'base-files:all=1\n' > "$artifact_dir/rootfs-debian-packages.lock"
	printf 'pip==1\n' > "$artifact_dir/rootfs-python.freeze"
	printf '{"schemaVersion":1,"packages":[]}\n' > "$artifact_dir/rootfs-debian-license-inventory.json"
	printf '{"schemaVersion":1,"distributions":[]}\n' > "$artifact_dir/rootfs-python-license-inventory.json"
	kernel_sha="$(sha256sum "$artifact_dir/kernel" | awk '{print $1}')"
	rootfs_source_sha="$(sha256sum "$artifact_dir/rootfs-source-manifest.json" | awk '{print $1}')"
	rootfs_contract_sha="$(sha256sum "$artifact_dir/secondbox-rootfs-contract.json" | awk '{print $1}')"
	debian_packages_sha="$(sha256sum "$artifact_dir/rootfs-debian-packages.lock" | awk '{print $1}')"
	python_freeze_sha="$(sha256sum "$artifact_dir/rootfs-python.freeze" | awk '{print $1}')"
	debian_licenses_sha="$(sha256sum "$artifact_dir/rootfs-debian-license-inventory.json" | awk '{print $1}')"
	python_licenses_sha="$(sha256sum "$artifact_dir/rootfs-python-license-inventory.json" | awk '{print $1}')"
	printf '{"artifactId":"test-runtime"}\n' > "$artifact_dir/runtime-manifest.json"
	printf '{"artifactId":"test-toolchain"}\n' > "$artifact_dir/toolchain-manifest.json"
	runtime_manifest_sha="$(sha256sum "$artifact_dir/runtime-manifest.json" | awk '{print $1}')"
	toolchain_manifest_sha="$(sha256sum "$artifact_dir/toolchain-manifest.json" | awk '{print $1}')"
	cat > "$artifact_dir/kernel-provenance.json" <<EOF
{"kernel":{"sha256":"$kernel_sha"}}
EOF
	kernel_provenance_sha="$(sha256sum "$artifact_dir/kernel-provenance.json" | awk '{print $1}')"
	cat > "$artifact_dir/manifest.json" <<EOF
{
  "artifactVersion": "test",
  "architecture": "amd64",
  "guestProtocol": {"minimum": 1, "maximum": 1},
  "runtimeBundle": {"artifactId":"test-runtime","path":"runtime-manifest.json","manifestDigest":"sha256:$runtime_manifest_sha","mandatoryGuestFeatures":[]},
  "toolchainBundle": {"artifactId":"test-toolchain","path":"toolchain-manifest.json","manifestDigest":"sha256:$toolchain_manifest_sha","mandatoryGuestFeatures":[]},
  "rootfs": {"path": "rootfs.ext4"},
  "kernel": {"path": "kernel"},
  "kernelProvenance": {"sha256": "$kernel_provenance_sha"},
  "rootfsSource": {"sha256": "$rootfs_source_sha"},
  "rootfsContract": {"sha256": "$rootfs_contract_sha", "state": "verified"},
  "rootfsProvenance": {
    "debianPackages": {"sha256": "$debian_packages_sha"},
    "pythonFreeze": {"sha256": "$python_freeze_sha"},
    "debianLicenses": {"sha256": "$debian_licenses_sha"},
    "pythonLicenses": {"sha256": "$python_licenses_sha"}
  },
  "shared": {"path": "shared.img"},
  "createdAt": "2026-07-12T00:00:00Z"
}
EOF
	(
		cd "$artifact_dir"
		sha256sum \
			kernel rootfs.ext4 shared.img kernel-provenance.json rootfs-source-manifest.json \
			secondbox-rootfs-contract.json rootfs-debian-packages.lock rootfs-python.freeze \
			rootfs-debian-license-inventory.json rootfs-python-license-inventory.json \
			runtime-manifest.json toolchain-manifest.json manifest.json > SHA256SUMS
	)
	openssl dgst -sha256 -sign "$work_dir/private.pem" -out "$artifact_dir/manifest.sig" "$artifact_dir/manifest.json"
}

clean_artifacts="$work_dir/clean-artifacts"
make_artifact_dir "$clean_artifacts" "$clean_image" "$clean_image"
"$script_dir/verify.sh" "$clean_artifacts" "$work_dir/public.pem" "$public_key_fingerprint"

unsigned_artifacts="$work_dir/unsigned-artifacts"
cp -a "$clean_artifacts" "$unsigned_artifacts"
rm "$unsigned_artifacts/manifest.sig"
if "$script_dir/verify.sh" "$unsigned_artifacts" "$work_dir/public.pem" "$public_key_fingerprint" >/dev/null 2>&1; then
	echo "expected unsigned artifact rejection" >&2
	exit 1
fi

incompatible_artifacts="$work_dir/incompatible-artifacts"
cp -a "$clean_artifacts" "$incompatible_artifacts"
sed -i 's/"architecture": "amd64"/"architecture": "incompatible"/' "$incompatible_artifacts/manifest.json"
(
	cd "$incompatible_artifacts"
	sed -i '/  manifest.json$/d' SHA256SUMS
	sha256sum manifest.json >> SHA256SUMS
)
openssl dgst -sha256 -sign "$work_dir/private.pem" -out "$incompatible_artifacts/manifest.sig" "$incompatible_artifacts/manifest.json"
if "$script_dir/verify.sh" "$incompatible_artifacts" "$work_dir/public.pem" "$public_key_fingerprint" >/dev/null 2>&1; then
	echo "expected incompatible signed architecture rejection" >&2
	exit 1
fi

bad_image="$work_dir/bad.ext4"
make_image "$bad_path_root" "$bad_image"
if "$script_dir/verify-browser-surface.sh" --rootfs "$bad_image" >/dev/null 2>&1; then
	echo "expected rootfs browser launcher rejection" >&2
	exit 1
fi
bad_artifacts="$work_dir/bad-artifacts"
make_artifact_dir "$bad_artifacts" "$bad_image" "$clean_image"
if "$script_dir/verify.sh" "$bad_artifacts" "$work_dir/public.pem" "$public_key_fingerprint" >/dev/null 2>&1; then
	echo "expected artifact verification browser launcher rejection" >&2
	exit 1
fi

bad_shared_root="$work_dir/bad-shared-root"
make_root "$bad_shared_root"
mkdir -p "$bad_shared_root/builtin-skills/browser"
printf '# Browser\n' > "$bad_shared_root/builtin-skills/browser/SKILL.md"
bad_shared_image="$work_dir/bad-shared.ext4"
make_image "$bad_shared_root" "$bad_shared_image"
if "$script_dir/verify-browser-surface.sh" --shared "$bad_shared_image" >/dev/null 2>&1; then
	echo "expected shared-image browser skill rejection" >&2
	exit 1
fi

echo "browser surface policy tests passed"
