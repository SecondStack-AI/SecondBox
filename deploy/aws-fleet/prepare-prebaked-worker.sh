#!/usr/bin/env bash
set -euo pipefail
umask 077
[[ $# == 2 ]] || { echo 'Usage: prepare-prebaked-worker.sh WORKER_BUNDLE_ARCHIVE EXPECTED_BUNDLE_SHA256' >&2; exit 2; }
archive=$1 expected=$2
[[ $(id -u) == 0 && $archive == /* && $expected =~ ^[a-f0-9]{64}$ ]] || {
  echo 'SecondBox pre-baked preparation requires root, an absolute archive and SHA256' >&2; exit 2;
}
[[ $(tar --version) == *'GNU tar'* ]] || { echo 'SecondBox pre-baked preparation requires GNU tar' >&2; exit 1; }
cache=/var/lib/secondbox-bootstrap
[[ $archive != "$cache"/* && ! -e $cache ]] || { echo 'SecondBox pre-baked preparation requires a fresh cache' >&2; exit 1; }
[[ ! -e /etc/secondbox/runner && ! -e /etc/secondstack/gateway && ! -e /srv/secondbox ]] || {
  echo 'SecondBox pre-baked preparation refuses an enrolled host or Workspace data' >&2; exit 1;
}
[[ -z $(docker ps --all --quiet) ]] || { echo 'SecondBox pre-baked preparation refuses existing Docker containers' >&2; exit 1; }
printf '%s  %s\n' "$expected" "$archive" | sha256sum --check --status
install -d -m 0700 "$cache"
install -d -m 0700 "$cache/tmp"
tar --extract --gzip --same-owner --same-permissions --numeric-owner --xattrs --xattrs-include='*' --acls \
  --delay-directory-restore --file "$archive" --directory "$cache"
test -x "$cache/install-worker.sh"
test -x "$cache/verify-prebaked-worker.sh"
"$cache/bin/secondbox-flat-root-digest" "$cache/artifacts/rootfs" "$cache/artifacts/materialization.json"
docker image load --input "$cache/worker-images.tar"
cp "$archive" "$cache/worker.tar.gz"
if [[ -d $cache/execution-images ]]; then
  printf 'archive\n' > "$cache/prebaked.execution-images"
  rm -rf -- "$cache/execution-images"
fi
printf '%s\n' "$expected" > "$cache/prebaked.bundle.sha256"
"$cache/verify-prebaked-worker.sh" "$cache" "$expected"
[[ -z $(docker ps --all --quiet) ]] || { echo 'SecondBox pre-baked preparation unexpectedly created containers' >&2; exit 1; }
printf 'SecondBox immutable cache prepared; no worker was enrolled and no Workspace disk was created.\n'
