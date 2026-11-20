#!/usr/bin/env python3
"""Build an identity-free worker AMI from an explicitly pinned bundle and placement."""
from datetime import datetime, timezone
from pathlib import Path
import argparse
import base64
import hashlib
import json
import os
import re
import secrets
import shlex
import subprocess
import time


def build_worker_ami(config_path, output_directory):
    os.umask(0o077)
    config = json.loads(config_path.read_text())
    patterns = {
        'accountId': r'[0-9]{12}', 'region': r'[a-z0-9-]+', 'baseAMI': r'ami-[0-9a-f]+',
        'baseAMIOwner': r'[0-9]{12}',
        'subnetId': r'subnet-[0-9a-f]+', 'securityGroupId': r'sg-[0-9a-f]+',
        'instanceProfile': r'[A-Za-z0-9+=,.@_-]+', 'fleetRef': r'[a-z0-9-]+',
        'instanceType': r'm7i\.(xlarge|2xlarge|4xlarge)',
        'imageName': r'[A-Za-z0-9._-]{3,128}', 'bundleBucket': r'[a-z0-9][a-z0-9.-]+[a-z0-9]',
        'bundleKey': r'[A-Za-z0-9._/-]+', 'bundleSHA256': r'[a-f0-9]{64}',
        'preparerKey': r'[A-Za-z0-9._/-]+',
    }
    for name, pattern in patterns.items():
        if not isinstance(config.get(name), str) or not re.fullmatch(pattern, config[name]):
            raise ValueError('SecondBox AMI configuration requires valid ' + name)
    if not 300 <= config['maximumBuildSeconds'] <= 7200:
        raise ValueError('SecondBox AMI build deadline must be 300–7200 seconds')
    if config['rootVolumeGiB'] != 32:
        raise ValueError('SecondBox AMI root volume must match the fleet template: 32 GiB')
    if config['preparerKey'] == config['bundleKey']:
        raise ValueError('SecondBox AMI preparer key must differ from the immutable bundle key')
    if not output_directory.is_absolute() or str(output_directory).startswith(('/tmp/', '/run/', '/dev/shm/')):
        raise ValueError('SecondBox AMI output must be an absolute persistent directory')
    output_directory.mkdir(mode=0o700)
    deadline = time.monotonic() + config['maximumBuildSeconds']
    receipt = {'config': config, 'startedAt': datetime.now(timezone.utc).isoformat(),
               'state': 'checking', 'launchToken': secrets.token_hex(16)}

    def persist():
        pending = output_directory / 'ami-build.pending.json'
        pending.write_text(json.dumps(receipt, indent=2) + '\n')
        pending.replace(output_directory / 'ami-build.json')

    def aws(*args):
        remaining = deadline - time.monotonic()
        if remaining < 1:
            raise TimeoutError('SecondBox AMI build exceeded its explicit deadline')
        result = subprocess.run(['aws', '--region', config['region'], *args, '--output', 'json'],
                                capture_output=True, text=True, timeout=min(remaining, 120))
        if result.returncode:
            raise RuntimeError('SecondBox AMI AWS operation failed: ' + ' '.join(args[:2]) + ': ' + result.stderr)
        return json.loads(result.stdout) if result.stdout.strip() else {}

    def wait_until(read, predicate, description):
        while True:
            result = read()
            if predicate(result):
                return result
            if time.monotonic() >= deadline:
                raise TimeoutError('SecondBox AMI timed out waiting for ' + description)
            time.sleep(5)

    def command(script):
        command_seconds = max(1, int(deadline - time.monotonic()))
        identifier = aws('ssm', 'send-command', '--instance-ids', receipt['instanceId'],
                         '--document-name', 'AWS-RunShellScript', '--parameters',
                         json.dumps({'commands': [script], 'executionTimeout': [str(command_seconds)]}))['Command']['CommandId']
        def invocation():
            return aws('ssm', 'list-command-invocations', '--command-id', identifier, '--details')['CommandInvocations']
        completed = wait_until(invocation, lambda rows: rows and rows[0]['Status'] in
                               {'Success', 'Cancelled', 'Failed', 'TimedOut', 'Cancelling'}, 'SSM command completion')[0]
        (output_directory / (identifier + '.json')).write_text(json.dumps(completed, indent=2) + '\n')
        if completed['Status'] != 'Success':
            raise RuntimeError('SecondBox AMI remote command failed: ' + identifier)
        return completed

    persist()
    if aws('sts', 'get-caller-identity')['Account'] != config['accountId']:
        raise ValueError('SecondBox AMI AWS account differs from the explicit configuration')
    base = aws('ec2', 'describe-images', '--image-ids', config['baseAMI'])['Images'][0]
    if (base['Architecture'] != 'x86_64' or base['State'] != 'available' or
            base['OwnerId'] != config['baseAMIOwner'] or base['PlatformDetails'] != 'Linux/UNIX'):
        raise ValueError('SecondBox AMI source must be available Linux amd64 from the explicit owner')
    receipt['baseImage'] = base
    persist()
    owned = aws('ec2', 'describe-instances', '--filters',
                'Name=tag:secondstack.ai/fleet-ref,Values=' + config['fleetRef'],
                'Name=tag:secondstack.ai/ami-builder,Values=true',
                'Name=instance-state-name,Values=pending,running,stopping,stopped')['Reservations']
    if owned:
        raise ValueError('SecondBox AMI build already has a live owned builder; inspect its receipt')
    tags = [{'Key': 'secondstack.ai/fleet-ref', 'Value': config['fleetRef']},
            {'Key': 'secondstack.ai/ami-builder', 'Value': 'true'},
            {'Key': 'Name', 'Value': config['imageName']}]
    preparer = Path(__file__).with_name('prepare-prebaked-worker.sh')
    preparer_sha = hashlib.sha256(preparer.read_bytes()).hexdigest()
    receipt['preparerSHA256'] = preparer_sha
    aws('s3api', 'put-object', '--bucket', config['bundleBucket'], '--key', config['preparerKey'],
        '--body', str(preparer))
    quote = shlex.quote
    bootstrap = f'''#!/bin/bash
set -euo pipefail
umask 077
shutdown -h +{(config['maximumBuildSeconds'] + 59) // 60}
export DEBIAN_FRONTEND=noninteractive
install -d -m 0700 /var/lib/secondbox-image-build/tmp
export TMPDIR=/var/lib/secondbox-image-build/tmp
sed -i 's|http://|https://|g' /etc/apt/sources.list.d/ubuntu.sources
apt-get update
apt-get install -y ca-certificates curl unzip docker.io docker-compose-v2 xfsprogs
curl --fail --location --proto '=https' --tlsv1.2 --retry 3 --output /var/lib/secondbox-image-build/aws.zip https://awscli.amazonaws.com/awscli-exe-linux-x86_64-2.38.0.zip
printf '%s  %s\\n' cb037d35cca5cefc3eb89ebabc471a0b4e4fe44135b72228c7eb0da2d2fae9f6 /var/lib/secondbox-image-build/aws.zip | sha256sum --check --status
unzip -q /var/lib/secondbox-image-build/aws.zip -d /var/lib/secondbox-image-build
/var/lib/secondbox-image-build/aws/install --bin-dir /usr/local/bin --install-dir /usr/local/aws-cli --update
aws s3 cp {quote('s3://' + config['bundleBucket'] + '/' + config['bundleKey'])} /var/lib/secondbox-image-build/worker.tar.gz --region {quote(config['region'])} --only-show-errors
aws s3 cp {quote('s3://' + config['bundleBucket'] + '/' + config['preparerKey'])} /var/lib/secondbox-image-build/prepare.sh --region {quote(config['region'])} --only-show-errors
printf '%s  %s\\n' {quote(preparer_sha)} /var/lib/secondbox-image-build/prepare.sh | sha256sum --check --status
bash /var/lib/secondbox-image-build/prepare.sh /var/lib/secondbox-image-build/worker.tar.gz {quote(config['bundleSHA256'])}
printf '%s\\n' {quote(config['bundleSHA256'])} > /var/lib/secondbox-image-build/ready.sha256
'''
    (output_directory / 'builder-user-data.sh').write_text(bootstrap)
    instance = None
    try:
        # Record an idempotent launch identity before EC2 can allocate anything.
        receipt['launchAttempted'] = True; persist()
        response = aws('ec2', 'run-instances', '--image-id', config['baseAMI'], '--instance-type', config['instanceType'],
                       '--client-token', receipt['launchToken'],
                       '--count', '1', '--subnet-id', config['subnetId'],
                       '--security-group-ids', config['securityGroupId'], '--associate-public-ip-address',
                       '--iam-instance-profile', json.dumps({'Name': config['instanceProfile']}),
                       '--metadata-options', json.dumps({'HttpTokens': 'required', 'HttpPutResponseHopLimit': 1}),
                       '--block-device-mappings', json.dumps([{'DeviceName': base['RootDeviceName'], 'Ebs': {
                           'VolumeSize': config['rootVolumeGiB'], 'VolumeType': 'gp3', 'Encrypted': True, 'DeleteOnTermination': True}}]),
                       '--tag-specifications', json.dumps([{'ResourceType': kind, 'Tags': tags} for kind in
                                                          ['instance', 'volume', 'network-interface']]),
                       '--user-data', 'file://' + str(output_directory / 'builder-user-data.sh'))
        instance = response['Instances'][0]['InstanceId']
        receipt.update(instanceId=instance, state='building'); persist()
        print('SecondBox AMI builder started:', instance, flush=True)
        wait_until(lambda: aws('ssm', 'describe-instance-information', '--filters',
                                json.dumps([{'Key':'InstanceIds','Values':[instance]}]))['InstanceInformationList'],
                   lambda rows: bool(rows) and rows[0]['PingStatus'] == 'Online', 'SSM registration')
        command('set -eu\ncloud-init status --wait\ntest "$(cat /var/lib/secondbox-image-build/ready.sha256)" = ' + quote(config['bundleSHA256']) +
                '\n/var/lib/secondbox-bootstrap/verify-prebaked-worker.sh /var/lib/secondbox-bootstrap ' + quote(config['bundleSHA256']) +
                '\ntest ! -e /etc/secondbox/runner\ntest ! -e /srv/secondbox\ntest -z "$(docker ps -aq)"\ndf -h /')
        # Return the SSM receipt first, then stop/scrub its own identity before powering off.
        seal = '''#!/bin/bash
set -euo pipefail
sleep 5
for unit in amazon-ssm-agent.service snap.amazon-ssm-agent.amazon-ssm-agent.service; do
  if systemctl cat "$unit" >/dev/null 2>&1; then systemctl stop "$unit"; fi
done
rm -rf /var/lib/amazon/ssm /var/log/amazon/ssm /var/snap/amazon-ssm-agent/common/* /root/.aws /var/lib/secondbox-image-build
rm -f /etc/ssh/ssh_host_*
cloud-init clean --logs --machine-id --seed
rm -f /root/secondbox-seal-image.sh
sync
systemctl poweroff
'''
        encoded = base64.b64encode(seal.encode()).decode()
        command('set -eu\nprintf %s ' + quote(encoded) + ' | base64 -d > /root/secondbox-seal-image.sh\n' +
                'chmod 0700 /root/secondbox-seal-image.sh\n' +
                'systemd-run --unit secondbox-seal-image --no-block /bin/bash /root/secondbox-seal-image.sh')
        wait_until(lambda: aws('ec2', 'describe-instances', '--instance-ids', instance)['Reservations'][0]['Instances'][0],
                   lambda item: item['State']['Name'] == 'stopped', 'identity scrub and poweroff')
        image = aws('ec2', 'create-image', '--instance-id', instance, '--name', config['imageName'], '--no-reboot',
                    '--tag-specifications', json.dumps([{'ResourceType': kind, 'Tags': tags +
                        [{'Key':'secondstack.ai/bundle-sha256','Value':config['bundleSHA256']}]} for kind in ['image','snapshot']]))['ImageId']
        receipt.update(imageId=image, state='snapshotting'); persist()
        available = wait_until(lambda: aws('ec2', 'describe-images', '--image-ids', image)['Images'][0],
                               lambda item: item['State'] in {'available','failed','error'}, 'AMI snapshot completion')
        if available['State'] != 'available':
            raise RuntimeError('SecondBox AMI snapshot failed: ' + image)
        receipt.update(state='available', image=available, completedAt=datetime.now(timezone.utc).isoformat()); persist()
        print('SecondBox pre-baked worker AMI available:', image, flush=True)
    except BaseException as error:
        receipt.update(state='failed', error=str(error)); persist()
        raise
    finally:
        if instance is None and receipt.get('launchAttempted'):
            # A CLI timeout can hide a successful launch. Recover only this request,
            # with bounded reads outside the build deadline reserved for cleanup.
            for attempt in range(6):
                try:
                    recovered = subprocess.run(['aws', '--region', config['region'], 'ec2', 'describe-instances',
                        '--filters', 'Name=client-token,Values=' + receipt['launchToken'],
                        'Name=tag:secondstack.ai/fleet-ref,Values=' + config['fleetRef'],
                        '--output', 'json'], capture_output=True, text=True, timeout=10)
                    if recovered.returncode:
                        raise RuntimeError(recovered.stderr)
                    instances = [item for reservation in json.loads(recovered.stdout)['Reservations']
                                 for item in reservation['Instances']]
                    if len(instances) > 1:
                        raise RuntimeError('SecondBox AMI launch token matched multiple instances')
                    if instances:
                        instance = instances[0]['InstanceId']
                        receipt.update(instanceId=instance, launchRecovered=True); persist()
                        break
                except (subprocess.TimeoutExpired, RuntimeError, ValueError) as error:
                    receipt['launchRecoveryError'] = str(error); persist()
                if attempt < 5:
                    time.sleep(5)
            if instance is None:
                receipt['launchRecoveryState'] = 'not_observed_inspect_launch_token'; persist()
        if instance:
            # Only this newly created, receipted builder is disposable; it has never held Workspaces.
            result = subprocess.run(['aws','--region',config['region'],'ec2','terminate-instances','--instance-ids',instance,'--output','json'],
                                    capture_output=True,text=True,timeout=60)
            if result.returncode:
                receipt['builderCleanupError'] = result.stderr; persist()
                raise RuntimeError('SecondBox AMI builder cleanup failed: ' + instance)
            receipt['builderTerminationRequested'] = True; persist()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--output-directory', type=Path, required=True)
    arguments = parser.parse_args()
    build_worker_ami(arguments.config, arguments.output_directory)
