#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat >&2 <<'USAGE'
Usage:
  verify-secondbox-rootfs.sh --live
  verify-secondbox-rootfs.sh --root-dir <prepared-rootfs-dir>
  verify-secondbox-rootfs.sh --rootfs <rootfs.ext4>
  verify-secondbox-rootfs.sh --prepared-root-dir <prepared-rootfs-dir>
  verify-secondbox-rootfs.sh --prepared-rootfs <rootfs.ext4>
  verify-secondbox-rootfs.sh --list-paths

Verifies commands and Python imports promised by the SecondBox guest rootfs
contract. --live executes probes; directory/image modes verify shipped paths.
USAGE
}

prepared_tool_paths() {
    cat <<'EOF'
/bin/cat
/bin/grep
/bin/ln
/bin/mkdir
/bin/mount
/bin/rm
/bin/sed
/bin/sh
/usr/bin/mountpoint
EOF
}

fail() {
    echo "SecondBox rootfs verification failed: $*" >&2
    exit 1
}

tool_paths() {
    cat <<'EOF'
/usr/bin/7z
/usr/bin/cmake
/usr/bin/convert
/usr/bin/csvcut
/usr/bin/curl
/usr/bin/diff
/usr/bin/dig
/usr/bin/dot
/usr/bin/fdfind
/usr/bin/ffmpeg
/usr/bin/file
/usr/bin/g++
/usr/bin/gcc
/usr/bin/git
/usr/bin/gs
/usr/bin/http
/usr/bin/jq
/usr/bin/less
/usr/bin/libreoffice
/usr/bin/make
/usr/bin/nc
/usr/bin/node
/usr/bin/npm
/usr/bin/pandoc
/usr/bin/patch
/usr/bin/pdftotext
/usr/bin/ping
/usr/bin/pkg-config
/usr/bin/python3
/usr/bin/qpdf
/usr/bin/rg
/usr/bin/shellcheck
/usr/bin/ssh
/usr/bin/sqlite3
/usr/bin/tesseract
/usr/bin/tree
/usr/bin/unzip
/usr/bin/wget
/usr/bin/wkhtmltopdf
/usr/bin/xmlstarlet
/usr/bin/yq
/usr/bin/zip
/usr/local/bin/uv
/usr/local/bin/uvx
EOF
}

live_commands() {
    cat <<'EOF'
7z
cmake
convert
csvcut
curl
diff
dig
dot
fdfind
ffmpeg
file
g++
gcc
git
gs
http
jq
less
libreoffice
make
nc
node
npm
pandoc
patch
pdftotext
ping
pkg-config
python3
qpdf
rg
shellcheck
ssh
sqlite3
tesseract
tree
unzip
uv
uvx
wget
wkhtmltopdf
xmlstarlet
yq
zip
EOF
}

verify_live() {
    while IFS= read -r command; do
        command -v "$command" >/dev/null 2>&1 || fail "missing command: $command"
    done < <(live_commands)
    python3 - <<'PY'
import aiohttp
import bs4
import duckdb
import fpdf
import httpx
import jinja2
import lxml
import markdown
import markdownify
import matplotlib
import numpy
import openpyxl
import pandas
import pdfplumber
import PIL
import pptx
import pyarrow
import PyPDF2
import reportlab
import rich
import scipy
import sklearn
import sympy
import tabulate
import tqdm
import xlsxwriter
PY
    echo "SecondBox rootfs verification passed: live root"
}

normalize_guest_path() {
    local path="$1"
    local component last_index
    local -a components=()
    local -a normalized=()
    [[ "$path" = /* ]] || fail "tool path must remain absolute: $path"
    IFS='/' read -r -a components <<<"$path"
    for component in "${components[@]}"; do
        case "$component" in
            ''|.) ;;
            ..)
                if [ "${#normalized[@]}" -gt 0 ]; then
                    last_index=$((${#normalized[@]} - 1))
                    unset "normalized[$last_index]"
                fi
                ;;
            *) normalized+=("$component") ;;
        esac
    done
    printf '/'
    if [ "${#normalized[@]}" -gt 0 ]; then
        local IFS=/
        printf '%s' "${normalized[*]}"
    fi
    printf '\n'
}

verify_root_dir_executable() {
    local root="$1"
    local original="$2"
    local current="$original"
    local candidate target
    local depth=0
    while [ "$depth" -lt 32 ]; do
        candidate="$root$current"
        if [ -L "$candidate" ]; then
            target="$(readlink -- "$candidate")"
            if [[ "$target" = /* ]]; then
                current="$(normalize_guest_path "$target")"
            else
                current="$(normalize_guest_path "$(dirname "$current")/$target")"
            fi
            depth=$((depth + 1))
            continue
        fi
        if [ -f "$candidate" ] && [ -x "$candidate" ]; then
            return 0
        fi
        fail "missing prepared-rootfs executable: $original (resolved to $current)"
    done
    fail "prepared-rootfs executable symlink depth exceeded: $original"
}

verify_root_dir_temporary_directory() {
    local root="$1"
    local mode owner
    mode="$(stat -c '%a' "$root/tmp")"
    owner="$(stat -c '%u:%g' "$root/tmp")"
    [ "$mode" = "1777" ] ||
        fail "prepared rootfs /tmp must have mode 1777, found $mode"
    [ "$owner" = "0:0" ] ||
        fail "prepared rootfs /tmp must be owned by root:root, found $owner"
}

verify_root_dir() {
    local root="$1"
    [ -d "$root" ] || { usage; exit 2; }
    while IFS= read -r path; do
        verify_root_dir_executable "$root" "$path"
    done < <(tool_paths)
    verify_root_dir_temporary_directory "$root"
    echo "SecondBox rootfs verification passed: prepared rootfs"
}

verify_prepared_root_dir() {
    local root="$1"
    [ -d "$root" ] || { usage; exit 2; }
    while IFS= read -r path; do
        verify_root_dir_executable "$root" "$path"
    done < <(prepared_tool_paths)
    verify_root_dir_temporary_directory "$root"
    [ -s "$root/var/lib/dpkg/status" ] ||
        fail "prepared OCI rootfs package inventory is unreadable"
    echo "SecondBox rootfs verification passed: prepared OCI rootfs"
}

verify_rootfs_executable() {
    local image="$1"
    local original="$2"
    local current="$original"
    local stat_output target
    local depth=0
    while [ "$depth" -lt 32 ]; do
        if ! stat_output="$(debugfs -R "stat $current" "$image" 2>&1)"; then
            fail "debugfs could not inspect rootfs executable: $current"
        fi
        if ! printf '%s\n' "$stat_output" | grep -q '^Inode:'; then
            fail "missing rootfs executable: $original (resolved to $current)"
        fi
        if printf '%s\n' "$stat_output" | grep -q 'Type: symlink'; then
            target="$(printf '%s\n' "$stat_output" | sed -n 's/^Fast link dest: "\(.*\)"$/\1/p')"
            [ -n "$target" ] || fail "unreadable rootfs executable symlink: $current"
            if [[ "$target" = /* ]]; then
                current="$(normalize_guest_path "$target")"
            else
                current="$(normalize_guest_path "$(dirname "$current")/$target")"
            fi
            depth=$((depth + 1))
            continue
        fi
        if ! printf '%s\n' "$stat_output" | grep -q 'Type: regular'; then
            fail "rootfs tool is not a regular executable: $original (resolved to $current)"
        fi
        if ! printf '%s\n' "$stat_output" | grep -Eq 'Mode: +0([1357][0-7]{2}|[0-7][1357][0-7]|[0-7]{2}[1357])'; then
            fail "non-executable rootfs tool: $original (resolved to $current)"
        fi
        return 0
    done
    fail "rootfs executable symlink depth exceeded: $original"
}

verify_rootfs_temporary_directory() {
    local image="$1"
    local stat_output
    stat_output="$(debugfs -R "stat /tmp" "$image" 2>&1)" ||
        fail "debugfs could not inspect rootfs /tmp"
    printf '%s\n' "$stat_output" | grep -Eq 'Type: directory +Mode: +01777' ||
        fail "rootfs /tmp must have mode 1777"
    printf '%s\n' "$stat_output" | grep -Eq '^User: +0 +Group: +0 ' ||
        fail "rootfs /tmp must be owned by root:root"
}

verify_rootfs() {
    local image="$1"
    [ -f "$image" ] || { usage; exit 2; }
    command -v debugfs >/dev/null 2>&1 || fail "debugfs is required to inspect rootfs"
    while IFS= read -r path; do
        verify_rootfs_executable "$image" "$path"
    done < <(tool_paths)
    verify_rootfs_temporary_directory "$image"
    echo "SecondBox rootfs verification passed: rootfs"
}

verify_prepared_rootfs() {
    local image="$1"
    [ -f "$image" ] || { usage; exit 2; }
    command -v debugfs >/dev/null 2>&1 || fail "debugfs is required to inspect rootfs"
    while IFS= read -r path; do
        verify_rootfs_executable "$image" "$path"
    done < <(prepared_tool_paths)
    verify_rootfs_temporary_directory "$image"
    if ! debugfs -R "stat /var/lib/dpkg/status" "$image" 2>&1 | grep -q '^Inode:'; then
        fail "prepared OCI rootfs package inventory is unreadable"
    fi
    echo "SecondBox rootfs verification passed: prepared OCI image"
}

case "${1:-}" in
    --live)
        [ "$#" -eq 1 ] || { usage; exit 2; }
        verify_live
        ;;
    --root-dir)
        [ "$#" -eq 2 ] || { usage; exit 2; }
        verify_root_dir "$2"
        ;;
    --rootfs)
        [ "$#" -eq 2 ] || { usage; exit 2; }
        verify_rootfs "$2"
        ;;
    --prepared-root-dir)
        [ "$#" -eq 2 ] || { usage; exit 2; }
        verify_prepared_root_dir "$2"
        ;;
    --prepared-rootfs)
        [ "$#" -eq 2 ] || { usage; exit 2; }
        verify_prepared_rootfs "$2"
        ;;
    --list-paths)
        [ "$#" -eq 1 ] || { usage; exit 2; }
        tool_paths
        ;;
    -h|--help)
        usage
        ;;
    *)
        usage
        exit 2
        ;;
esac
