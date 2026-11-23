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
data=/srv/secondbox
[[ $archive != "$cache"/* && ! -e $cache ]] || { echo 'SecondBox pre-baked preparation requires a fresh cache' >&2; exit 1; }
[[ ! -e /etc/secondbox/runner && ! -e /etc/secondstack/gateway && ! -e $data ]] || {
  echo 'SecondBox pre-baked preparation refuses an enrolled host or Workspace data' >&2; exit 1;
}
[[ -z $(docker ps --all --quiet) ]] || { echo 'SecondBox pre-baked preparation refuses existing Docker containers' >&2; exit 1; }
root_source=$(findmnt -n -o SOURCE /)
root_parent=$(lsblk -ndo PKNAME "$root_source")
[[ -n $root_parent ]] || root_parent=$(basename "$root_source")
mapfile -t data_disks < <(lsblk -dpno NAME,TYPE | awk -v root="/dev/$root_parent" '$2 == "disk" && $1 != root {print $1}')
[[ ${#data_disks[@]} == 1 ]] || { echo 'SecondBox pre-baked preparation requires exactly one non-root data disk' >&2; exit 1; }
data_disk=${data_disks[0]}
[[ -z $(blkid -s TYPE -o value "$data_disk" || true) ]] || { echo 'SecondBox pre-baked preparation requires a blank data disk' >&2; exit 1; }
printf '%s  %s\n' "$expected" "$archive" | sha256sum --check --status
install -d -m 0700 "$cache"
install -d -m 0700 "$cache/tmp"
tar --extract --gzip --same-owner --same-permissions --numeric-owner --xattrs --xattrs-include='*' --acls \
  --delay-directory-restore --file "$archive" --directory "$cache"
test -x "$cache/install-worker.sh"
test -x "$cache/verify-prebaked-worker.sh"
"$cache/bin/secondbox-flat-root-digest" "$cache/artifacts/rootfs" "$cache/artifacts/materialization.json"
docker image load --input "$cache/worker-images.tar"

# The data disk becomes the worker data snapshot: the base artifacts and the
# execution-image cache are already in place, owned by the image fetcher, and
# the Runner has recorded their verification. A worker created from it hashes
# and copies nothing at boot. Ownership is set before recording because chown
# changes the change time the records bind.
mkfs.xfs -m reflink=1 "$data_disk"
install -d -m 0700 "$data"
mount "$data_disk" "$data"
install -d -m 0700 "$data/state/workspaces" "$data/state/log" "$data/state/execution-images/tmp"
cp -a "$cache/artifacts" "$data/artifacts"
"$cache/bin/secondbox-flat-root-digest" "$data/artifacts/rootfs" "$data/artifacts/materialization.json"
"$cache/stage-worker-execution-cache.sh" "$cache" "$data/state"
chown -R 10002:10002 "$data/state/execution-images"
if [[ -d $cache/execution-images ]]; then
  mapfile -t runner_images < <(sed -n 's/^SECONDBOX_RUNNER_IMAGE=//p' "$cache/runtime.env")
  [[ ${#runner_images[@]} == 1 && ${runner_images[0]} =~ ^sha256:[a-f0-9]{64}$ ]] || {
    echo 'SecondBox pre-baked preparation requires one immutable SECONDBOX_RUNNER_IMAGE' >&2; exit 1;
  }
  # Container paths match the worker compose.yaml: records name them.
  docker run --rm --network none --user 0:0 --entrypoint /usr/local/bin/secondbox-runner \
    --volume "$data:/var/lib/secondbox" \
    --env SECONDBOX_RUNNER_EXECUTION_IMAGE_CACHE_ROOT=/var/lib/secondbox/state/execution-images \
    --env SECONDBOX_RUNNER_EXECUTION_IMAGE_VERIFICATION_ROOT=/var/lib/secondbox/state/execution-image-verifications \
    "${runner_images[0]}" -record-execution-image-verifications
  rm -rf -- "$cache/execution-images"
fi
printf '%s\n' "$expected" > "$data/.secondbox-prebaked"
sync
umount "$data"
rmdir "$data"
printf '%s\n' "$expected" > "$cache/prebaked.bundle.sha256"
"$cache/verify-prebaked-worker.sh" "$cache" "$expected"
[[ -z $(docker ps --all --quiet) ]] || { echo 'SecondBox pre-baked preparation unexpectedly created containers' >&2; exit 1; }
printf 'SecondBox immutable cache and worker data disk prepared; no worker was enrolled and no Workspace was created.\n'
