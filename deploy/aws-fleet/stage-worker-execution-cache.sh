#!/usr/bin/env bash
set -euo pipefail
umask 077
[[ $# == 3 ]] || { echo 'Usage: stage-worker-execution-cache.sh BUNDLE_DIRECTORY STATE_DIRECTORY prebaked|download' >&2; exit 2; }
bundle=$1 state=$2 mode=$3
[[ $bundle == /* && $state == /* && -d $state/execution-images ]] || {
  echo 'SecondBox execution-cache staging requires absolute bundle/state directories and an existing cache destination' >&2; exit 2;
}
staging=$(mktemp -d "$state/.worker-cache.XXXXXXXX")
trap 'rm -rf -- "$staging"' EXIT
case "$mode" in
  prebaked)
    if [[ -f $bundle/prebaked.execution-images ]]; then
      [[ $(cat "$bundle/prebaked.execution-images") == archive ]] || { echo 'SecondBox execution-cache staging marker is invalid' >&2; exit 1; }
      tar --extract --gzip --same-owner --same-permissions --numeric-owner \
        --xattrs --xattrs-include='*' --acls --delay-directory-restore \
        --file "$bundle/worker.tar.gz" --directory "$staging" execution-images
    fi
    ;;
  download)
    if [[ -d $bundle/execution-images ]]; then
      cp --reflink=never --sparse=always -a "$bundle/execution-images" "$staging/execution-images"
    fi
    ;;
  *) echo 'SecondBox execution-cache staging mode must be prebaked or download' >&2; exit 2 ;;
esac
[[ -d $staging/execution-images ]] || exit 0
cache="$state/execution-images"
mkdir -p "$cache/.locks"
for image in "$staging/execution-images"/*; do
  digest=${image##*/}
  [[ $digest =~ ^[a-f0-9]{64}$ && -d $image && ! -L $image ]] || continue
  [[ ! -e $cache/$digest ]] || continue
  (
    # Publish a complete verified directory under the Runner's digest lock.
    # Eviction can unlink lock files; retry a descriptor whose inode moved.
    lock="$cache/.locks/$digest.lock"
    while :; do
      exec {descriptor}>>"$lock"
      flock --exclusive "$descriptor"
      [[ $lock -ef /proc/self/fd/$descriptor ]] && break
      exec {descriptor}>&-
    done
    [[ ! -e $cache/$digest ]] || exit 0
    (cd "$image" && sha256sum --check --strict SHA256SUMS)
    mv --no-target-directory "$image" "$cache/$digest"
  )
done
