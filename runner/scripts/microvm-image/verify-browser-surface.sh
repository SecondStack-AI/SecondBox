#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat >&2 <<'USAGE'
Usage:
  verify-browser-surface.sh --root-dir <prepared-rootfs-dir>
  verify-browser-surface.sh --rootfs <rootfs.ext4>
  verify-browser-surface.sh --shared <shared.img>

Fails when a standard tool-VM filesystem contains a retired browser skill,
browser launcher/runtime, Chromium/Firefox package, or compiled browser entry
point. Artifact verification requires the filesystem-specific inspection tool.
USAGE
}

fail() {
    echo "browser surface policy violation: $*" >&2
    exit 1
}

rootfs_forbidden_paths() {
    cat <<'EOF'
/app/dist/browser
/app/dist/browser.js
/app/dist/browser.js.map
/app/dist/browser.d.ts
/app/dist/browser.d.ts.map
/app/dist/browser-wrapper.js
/app/dist/browser-wrapper.js.map
/app/dist/browser-wrapper.d.ts
/app/dist/browser-wrapper.d.ts.map
/app/dist/browserd.js
/app/dist/browserd.js.map
/app/dist/browserd.d.ts
/app/dist/browserd.d.ts.map
/app/node_modules/playwright
/app/node_modules/playwright-core
/app/node_modules/@playwright
/app/node_modules/.bin/playwright
/app/src/browser
/app/src/browser-wrapper.ts
/app/src/browserd.ts
/builtin-skills/browser
/etc/chromium
/etc/chromium.d
/home/agent/.cache/ms-playwright
/opt/google/chrome
/opt/ms-playwright
/root/.cache/ms-playwright
/usr/bin/chromium
/usr/bin/chromium-browser
/usr/bin/firefox
/usr/bin/firefox-esr
/usr/bin/google-chrome
/usr/bin/google-chrome-stable
/usr/bin/playwright
/usr/lib/chromium
/usr/lib/firefox
/usr/lib/firefox-esr
/usr/local/bin/browser
/usr/local/bin/browserd
/usr/local/bin/chromium
/usr/local/bin/chromium-browser
/usr/local/bin/firefox
/usr/local/bin/firefox-esr
/usr/local/bin/google-chrome
/usr/local/bin/google-chrome-stable
/usr/local/bin/playwright
/usr/local/lib/node_modules/playwright
/usr/local/lib/node_modules/playwright-core
/usr/local/lib/node_modules/@playwright
/usr/local/lib/python3.11/site-packages/playwright
/usr/lib/node_modules/playwright
/usr/lib/node_modules/playwright-core
/usr/lib/node_modules/@playwright
/usr/lib/python3/dist-packages/playwright
/var/cache/ms-playwright
/var/lib/chromium
EOF
}

shared_forbidden_paths() {
    cat <<'EOF'
/builtin-skills/browser
EOF
}

verify_package_status() {
    status="$1"
    if printf '%s\n' "$status" | grep -Eq '^Package: (chromium|firefox|google-chrome|playwright)(-[^ ]+)?$'; then
        package="$(printf '%s\n' "$status" | grep -E '^Package: (chromium|firefox|google-chrome|playwright)(-[^ ]+)?$' | head -n 1 | sed 's/^Package: //')"
        fail "installed browser package: $package"
    fi
}

verify_root_dir() {
    root="$1"
    [ -d "$root" ] || { usage; exit 2; }
    while IFS= read -r path; do
        if [ -e "$root$path" ] || [ -L "$root$path" ]; then
            fail "forbidden prepared-rootfs path: $path"
        fi
    done < <(rootfs_forbidden_paths)
    [ -s "$root/var/lib/dpkg/status" ] || fail "prepared-rootfs package inventory is unreadable"
    verify_package_status "$(cat "$root/var/lib/dpkg/status")"
    echo "browser surface policy passed: prepared rootfs"
}

ext_path_exists() {
    image="$1"
    path="$2"
    debugfs -R "stat $path" "$image" 2>/dev/null | grep -q '^Inode:'
}

erofs_path_exists() {
    image="$1"
    path="$2"
    dump.erofs "--path=$path" "$image" 2>/dev/null | grep -q '^Path : '
}

squashfs_path_exists() {
    image="$1"
    path="$2"
    relative="${path#/}"
    unsquashfs -ll "$image" "$relative" 2>/dev/null | grep -Eq "squashfs-root/${relative}(/|$)"
}

image_format() {
    description="$(file -b "$1")"
    case "$description" in
        *"ext2 filesystem"*|*"ext3 filesystem"*|*"ext4 filesystem"*) echo ext ;;
        *"EROFS filesystem"*) echo erofs ;;
        *"Squashfs filesystem"*) echo squashfs ;;
        *) fail "unsupported filesystem image: $1 ($description)" ;;
    esac
}

require_image_tool() {
    format="$1"
    case "$format" in
        ext) tool=debugfs ;;
        erofs) tool=dump.erofs ;;
        squashfs) tool=unsquashfs ;;
    esac
    command -v "$tool" >/dev/null 2>&1 || fail "$tool is required to inspect $format images"
}

image_path_exists() {
    format="$1"
    image="$2"
    path="$3"
    case "$format" in
        ext) ext_path_exists "$image" "$path" ;;
        erofs) erofs_path_exists "$image" "$path" ;;
        squashfs) squashfs_path_exists "$image" "$path" ;;
    esac
}

verify_image() {
    kind="$1"
    image="$2"
    [ -f "$image" ] || { usage; exit 2; }
    command -v file >/dev/null 2>&1 || fail "file is required to identify filesystem images"
    format="$(image_format "$image")"
    require_image_tool "$format"

    if [ "$kind" = rootfs ]; then
        paths="$(rootfs_forbidden_paths)"
    else
        paths="$(shared_forbidden_paths)"
    fi
    while IFS= read -r path; do
        if image_path_exists "$format" "$image" "$path"; then
            fail "forbidden $kind path: $path"
        fi
    done <<<"$paths"

    if [ "$kind" = rootfs ]; then
        [ "$format" = ext ] || fail "rootfs must be ext4, got $format"
        status="$(debugfs -R 'cat /var/lib/dpkg/status' "$image" 2>/dev/null || true)"
        [ -n "$status" ] || fail "rootfs package inventory is unreadable"
        verify_package_status "$status"
    fi
    echo "browser surface policy passed: $kind ($format)"
}

case "${1:-}" in
    --root-dir)
        [ "$#" -eq 2 ] || { usage; exit 2; }
        verify_root_dir "$2"
        ;;
    --rootfs)
        [ "$#" -eq 2 ] || { usage; exit 2; }
        verify_image rootfs "$2"
        ;;
    --shared)
        [ "$#" -eq 2 ] || { usage; exit 2; }
        verify_image shared "$2"
        ;;
    -h|--help)
        usage
        ;;
    *)
        usage
        exit 2
        ;;
esac
