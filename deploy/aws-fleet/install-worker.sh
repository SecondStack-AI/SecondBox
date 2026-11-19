#!/usr/bin/env bash
set -euo pipefail
umask 077

region= enrollment_url= fleet=
while (($#)); do
  case "$1" in
    --region) region=$2; shift 2 ;;
    --enrollment-url) enrollment_url=$2; shift 2 ;;
    --fleet) fleet=$2; shift 2 ;;
    *) echo "SecondBox worker bootstrap: unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ $region =~ ^[a-z0-9-]+$ && $enrollment_url =~ ^https://[a-zA-Z0-9.:/-]+$ && $fleet =~ ^[a-z][a-z0-9-]{2,63}$ ]] || {
  echo 'SecondBox worker bootstrap requires region, HTTPS enrollment URL and fleet ownership' >&2; exit 2;
}
[[ $(id -u) == 0 ]] || { echo 'SecondBox worker bootstrap requires root' >&2; exit 1; }
bundle=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root_source=$(findmnt -n -o SOURCE /)
root_parent=$(lsblk -ndo PKNAME "$root_source")
[[ -n $root_parent ]] || root_parent=$(basename "$root_source")
mapfile -t data_disks < <(lsblk -dpno NAME,TYPE | awk -v root="/dev/$root_parent" '$2 == "disk" && $1 != root {print $1}')
[[ ${#data_disks[@]} == 1 ]] || { echo 'SecondBox worker bootstrap requires exactly one non-root data disk' >&2; exit 1; }
data_disk=${data_disks[0]}
filesystem=$(blkid -s TYPE -o value "$data_disk" || true)
new_filesystem=false
if [[ -z $filesystem ]]; then
  mkfs.xfs -m reflink=1 "$data_disk"
  new_filesystem=true
elif [[ $filesystem != xfs ]]; then
  echo 'SecondBox worker bootstrap refuses a foreign data filesystem' >&2; exit 1
fi
install -d -m 0700 /srv/secondbox
if ! mountpoint -q /srv/secondbox; then mount "$data_disk" /srv/secondbox; fi
[[ $(findmnt -n -o SOURCE /srv/secondbox) == "$data_disk" ]] || { echo 'SecondBox worker bootstrap data mount differs' >&2; exit 1; }
marker=/srv/secondbox/.secondbox-fleet
if [[ $new_filesystem == true ]]; then
  printf '%s\n' "$fleet" > "$marker"
elif [[ ! -f $marker || $(cat "$marker") != "$fleet" ]]; then
  echo 'SecondBox worker bootstrap refuses an unowned retained data volume' >&2; exit 1
fi
filesystem_uuid=$(blkid -s UUID -o value "$data_disk")
[[ -n $filesystem_uuid ]] || { echo 'SecondBox worker bootstrap data filesystem has no UUID' >&2; exit 1; }
mapfile -t mount_sources < <(awk '$2 == "/srv/secondbox" && $1 !~ /^#/ {print $1}' /etc/fstab)
if [[ ${#mount_sources[@]} == 0 ]]; then
  printf 'UUID=%s /srv/secondbox xfs defaults 0 0\n' "$filesystem_uuid" >> /etc/fstab
elif [[ ${#mount_sources[@]} != 1 || ${mount_sources[0]} != "UUID=$filesystem_uuid" ]]; then
  echo 'SecondBox worker bootstrap refuses a conflicting persistent data mount' >&2; exit 1
fi
# Docker may restart the containers before cloud-init runs on the next boot.
# It must first mount durable data and recreate the fetcher's ephemeral socket
# directory with its owning UID, or containers could write on the root disk.
install -d -m 0755 /etc/systemd/system/docker.service.d
cat > /etc/systemd/system/docker.service.d/secondbox-workspace.conf <<'EOF'
[Unit]
RequiresMountsFor=/srv/secondbox

[Service]
ExecStartPre=/usr/bin/install -d -m 0700 -o 10002 -g 10002 /run/secondbox-image-fetcher
EOF
systemctl daemon-reload
export TMPDIR=/srv/secondbox/tmp
install -d -m 0700 "$TMPDIR" /srv/secondbox/state/workspaces /srv/secondbox/state/log /srv/secondbox/state/execution-images/tmp /etc/secondbox/runner /etc/secondstack/gateway /etc/secondbox/image-registry /run/secondbox-image-fetcher /etc/secondbox/runner/agent-gateway
install -m 0755 "$bundle/bin/secondbox-fleet-enroll" /usr/local/bin/secondbox-fleet-enroll
install -m 0600 "$bundle/enrollment-ca.pem" /etc/secondbox/enrollment-ca.pem
/usr/local/bin/secondbox-fleet-enroll --endpoint "$enrollment_url" --region "$region" --ca-file "$bundle/enrollment-ca.pem" --output /etc/secondbox/runner
install -m 0600 /etc/secondbox/runner/client.crt /etc/secondstack/gateway/client.crt
install -m 0600 /etc/secondbox/runner/gateway.key /etc/secondstack/gateway/client.key
install -m 0600 /etc/secondbox/runner/workload-ca.crt /etc/secondstack/gateway/workload-ca.crt
# Renew leaves on the host. Both consumers mount directories and reload the
# same-key leaf on new TLS handshakes, so existing guests and tunnels continue.
cat > /etc/systemd/system/secondbox-fleet-renew.service <<EOF
[Unit]
Description=Renew SecondBox fleet worker certificate leaves
Wants=network-online.target
After=network-online.target
RequiresMountsFor=/srv/secondbox
StartLimitIntervalSec=300
StartLimitBurst=3

[Service]
Type=oneshot
UMask=0077
Environment=TMPDIR=/srv/secondbox/tmp
ExecStart=/usr/local/bin/secondbox-fleet-enroll --renew --endpoint $enrollment_url --region $region --ca-file /etc/secondbox/enrollment-ca.pem --output /etc/secondbox/runner --gateway-certificate-copy /etc/secondstack/gateway/client.crt
TimeoutStartSec=210
Restart=on-failure
RestartSec=60
EOF
cat > /etc/systemd/system/secondbox-fleet-renew.timer <<'EOF'
[Unit]
Description=Keep SecondBox fleet worker certificate leaves current

[Timer]
OnBootSec=2min
OnUnitInactiveSec=5min
RandomizedDelaySec=30
Unit=secondbox-fleet-renew.service

[Install]
WantedBy=timers.target
EOF
systemctl daemon-reload
systemctl enable --now secondbox-fleet-renew.timer
cp -a /etc/secondbox/runner/image-registry/. /etc/secondbox/image-registry/
test -f /etc/secondbox/image-registry/tenants.json
if [[ ! -d /srv/secondbox/artifacts ]]; then
  cp -a "$bundle/artifacts" /srv/secondbox/artifacts
fi
"$bundle/bin/secondbox-flat-root-digest" /srv/secondbox/artifacts/rootfs /srv/secondbox/artifacts/materialization.json
cp --reflink=always /srv/secondbox/artifacts/materialization.json /srv/secondbox/reflink-probe
rm /srv/secondbox/reflink-probe
docker image load --input "$bundle/worker-images.tar"
if [[ -d $bundle/execution-images ]]; then
  cp -an "$bundle/execution-images/." /srv/secondbox/state/execution-images/
fi
chown -R 10002:10002 /srv/secondbox/state/execution-images /run/secondbox-image-fetcher /etc/secondbox/image-registry
install -m 0600 "$bundle/runtime.env" /etc/secondbox/runtime.env
install -m 0644 "$bundle/compose.yaml" /etc/secondbox/compose.yaml
docker compose --project-name secondbox-elastic-worker \
  --env-file /etc/secondbox/runner/runner.env \
  --env-file /etc/secondbox/runner/agent-runner-gateway.env \
  --env-file /etc/secondbox/runtime.env \
  -f /etc/secondbox/compose.yaml up --detach --wait --wait-timeout 180
