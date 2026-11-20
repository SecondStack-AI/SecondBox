#!/usr/bin/env bash
set -euo pipefail
[[ $# == 3 ]] || { echo 'Usage: stage-worker-execution-cache.sh BUNDLE_DIRECTORY STATE_DIRECTORY prebaked|download' >&2; exit 2; }
bundle=$1 state=$2 mode=$3
[[ $bundle == /* && $state == /* && -d $state/execution-images ]] || {
  echo 'SecondBox execution-cache staging requires absolute bundle/state directories and an existing cache destination' >&2; exit 2;
}
case "$mode" in
  prebaked)
    if [[ -f $bundle/prebaked.execution-images ]]; then
      [[ $(cat "$bundle/prebaked.execution-images") == archive ]] || { echo 'SecondBox execution-cache staging marker is invalid' >&2; exit 1; }
      # A retry may overlap live cache users. Never replace their files, lock
      # inodes or directory ownership; the Runner owns cache mutation and repair.
      tar --extract --gzip --skip-old-files --same-owner --same-permissions --numeric-owner \
        --xattrs --xattrs-include='*' --acls --delay-directory-restore \
        --file "$bundle/worker.tar.gz" --directory "$state" execution-images
    fi
    ;;
  download)
    if [[ -d $bundle/execution-images ]]; then
      cp -an "$bundle/execution-images/." "$state/execution-images/"
    fi
    ;;
  *) echo 'SecondBox execution-cache staging mode must be prebaked or download' >&2; exit 2 ;;
esac
