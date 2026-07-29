#!/bin/sh
set -eu

found_source=0
for source_file in \
    /etc/apt/sources.list \
    /etc/apt/sources.list.d/*.list \
    /etc/apt/sources.list.d/*.sources
do
    [ -f "$source_file" ] || continue
    while IFS= read -r source_line; do
        case "$source_line" in
            deb\ *|deb-src\ *|URIs:\ *)
                found_source=1
                printf '%s\n' "$source_line" |
                    grep -Eq 'https://snapshot\.debian\.org/archive/debian/[0-9]{8}T[0-9]{6}Z/?([[:space:]]|$)' ||
                    {
                        echo "SecondBox rootfs build failed: non-snapshot Debian source in $source_file" >&2
                        exit 2
                    }
                ;;
        esac
    done < "$source_file"
done

[ "$found_source" -eq 1 ] ||
    {
        echo "SecondBox rootfs build failed: no dated Debian snapshot source configured" >&2
        exit 2
    }
