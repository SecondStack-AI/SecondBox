#!/bin/sh
# Debian ships ImageMagick with the ghostscript-delegated coders (PDF/PS/EPS/XPS)
# disabled (rights="none") as a CVE mitigation. Agents routinely need
# `convert file.pdf out.png` and similar, so re-enable read|write for those coders
# in the tool-VM sandbox (ephemeral, per-operation — the blast radius is a single
# tool call, not the host).
set -eu

relaxed=0
for policy in /etc/ImageMagick-6/policy.xml /etc/ImageMagick-7/policy.xml; do
    [ -f "$policy" ] || continue
    sed -i -E 's/rights="none" pattern="(PS|PS2|PS3|EPS|PDF|XPS)"/rights="read|write" pattern="\1"/g' "$policy"
    echo "relaxed ghostscript-delegate coders in $policy"
    relaxed=1
done

if [ "$relaxed" -eq 0 ]; then
    echo "no ImageMagick policy.xml found; skipping (imagemagick may not be installed)" >&2
fi
