"""Fault-boundary tests for the worker AMI provisioning CLI; no AWS calls."""
import importlib.util
import json
import os
import signal
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
source = Path(__file__).resolve().parents[2] / 'deploy/aws-fleet/build-worker-ami.py'
spec = importlib.util.spec_from_file_location('worker_ami', source)
worker_ami = importlib.util.module_from_spec(spec)
spec.loader.exec_module(worker_ami)


class WorkerAMIFailureBoundaries(unittest.TestCase):
    def setUp(self):
        test_root = source.parents[2] / '.tmp/worker-ami-tests'
        test_root.mkdir(parents=True, exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(dir=test_root)
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.config = {
            'accountId': '123456789012', 'region': 'us-east-1', 'baseAMI': 'ami-abcdef',
            'baseAMIOwner': '123456789012', 'subnetId': 'subnet-abcdef',
            'securityGroupId': 'sg-abcdef', 'instanceProfile': 'builder',
            'fleetRef': 'test-fleet', 'instanceType': 'm7i.xlarge', 'imageName': 'test-worker',
            'bundleBucket': 'test-bucket', 'bundleKey': 'artifacts/worker.tar.gz',
            'bundleSHA256': 'a' * 64, 'preparerKey': 'artifacts/prepare.sh',
            'maximumBuildSeconds': 7200, 'rootVolumeGiB': 32,
        }
        self.calls = []
        self.parameters = []
        self.launch_token = None
        self.ambiguous_launch = False
        self.recovery_reads = 0
        self.seal_acknowledged = True
        self.extra_base_disk = False
        self.extra_builder_disk = False
        self.interrupt_readiness = False
        self.interrupt_termination = False

    def run_builder(self):
        config_path = self.root / 'config.json'
        config_path.write_text(json.dumps(self.config))
        with patch.object(worker_ami.subprocess, 'run', self.aws), patch.object(worker_ami.time, 'sleep'):
            worker_ami.build_worker_ami(config_path, self.root / 'output')

    def aws(self, arguments, **kwargs):
        service, operation = arguments[3:5]
        self.calls.append((service, operation, arguments))
        result = {}
        if operation == 'get-caller-identity':
            result = {'Account': self.config['accountId']}
        elif operation == 'describe-images':
            result = {'Images': [{'Architecture': 'x86_64', 'State': 'available',
                                  'OwnerId': self.config['baseAMIOwner'],
                                  'PlatformDetails': 'Linux/UNIX', 'RootDeviceName': '/dev/sda1',
                                  'RootDeviceType': 'ebs',
                                  'BlockDeviceMappings': [{'DeviceName': '/dev/sda1', 'Ebs': {'SnapshotId': 'snap-abcdef'}}] +
                                      ([{'DeviceName': '/dev/sdf', 'Ebs': {'SnapshotId': 'snap-fedcba'}}] if self.extra_base_disk else [])}]}
        elif operation == 'describe-instances':
            if '--instance-ids' in arguments:
                result = {'Reservations': [{'Instances': [{'InstanceId': 'i-abcdef', 'State': {'Name': 'stopped'},
                    'BlockDeviceMappings': [{'DeviceName': '/dev/sda1', 'Ebs': {'VolumeId': 'vol-abcdef'}}] +
                        ([{'DeviceName': '/dev/sdf', 'Ebs': {'VolumeId': 'vol-fedcba'}}] if self.extra_builder_disk else [])}]}]}
            elif any('Name=client-token,' in value for value in arguments):
                self.assertIn('Name=client-token,Values=' + self.launch_token, arguments)
                self.assertIn('Name=tag:secondstack.ai/fleet-ref,Values=test-fleet', arguments)
                self.recovery_reads += 1
                # Exercise eventual visibility of a launch whose response was lost.
                result = {'Reservations': [] if self.recovery_reads == 1 else [{'Instances': [{'InstanceId': 'i-abcdef'}]}]}
            else:
                result = {'Reservations': []}
        elif operation == 'run-instances':
            self.launch_token = arguments[arguments.index('--client-token') + 1]
            receipt = json.loads((self.root / 'output/ami-build.json').read_text())
            self.assertEqual(receipt['launchToken'], self.launch_token)
            self.assertTrue(receipt['launchAttempted'])
            if self.ambiguous_launch:
                raise subprocess.TimeoutExpired(arguments, 120)
            result = {'Instances': [{'InstanceId': 'i-abcdef'}]}
        elif operation == 'describe-instance-information':
            if self.interrupt_readiness:
                os.kill(os.getpid(), signal.SIGTERM)
            result = {'InstanceInformationList': [{'PingStatus': 'Online'}]}
        elif operation == 'get-console-output':
            receipt = json.loads((self.root / 'output/ami-build.json').read_text())
            result = {'Output': 'SecondBox AMI sealed: ' + (receipt['sealToken'] if self.seal_acknowledged else '0' * 32)}
        elif operation == 'send-command':
            self.parameters.append(json.loads(arguments[arguments.index('--parameters') + 1]))
            result = {'Command': {'CommandId': 'command-' + str(len(self.parameters))}}
        elif operation == 'list-command-invocations':
            result = {'CommandInvocations': [{'Status': 'Success'}]}
        elif operation == 'create-image':
            result = {'ImageId': 'ami-fedcba'}
        elif operation == 'terminate-instances' and self.interrupt_termination:
            os.kill(os.getpid(), signal.SIGTERM)
        elif operation not in {'put-object', 'terminate-instances'}:
            self.fail('Unexpected AWS call: ' + str(arguments))
        return subprocess.CompletedProcess(arguments, 0, json.dumps(result), '')

    def test_same_object_key_fails_before_any_aws_write(self):
        self.config['preparerKey'] = self.config['bundleKey']
        with self.assertRaisesRegex(ValueError, 'preparer key must differ'):
            self.run_builder()
        self.assertEqual(self.calls, [])

    def test_root_larger_than_fleet_template_fails_before_launch(self):
        self.config['rootVolumeGiB'] = 40
        with self.assertRaisesRegex(ValueError, 'fleet template: 32 GiB'):
            self.run_builder()
        self.assertEqual(self.calls, [])

    def test_ambiguous_launch_is_recovered_and_terminated(self):
        self.ambiguous_launch = True
        with self.assertRaises(subprocess.TimeoutExpired):
            self.run_builder()
        receipt = json.loads((self.root / 'output/ami-build.json').read_text())
        self.assertEqual(receipt['state'], 'failed')
        self.assertTrue(receipt['launchRecovered'])
        self.assertEqual(self.recovery_reads, 2)
        self.assertTrue(receipt['builderTerminationRequested'])
        terminated = [arguments for _, operation, arguments in self.calls if operation == 'terminate-instances']
        self.assertEqual(len(terminated), 1)
        self.assertEqual(terminated[0][terminated[0].index('--instance-ids') + 1], 'i-abcdef')

    def test_readiness_uses_remaining_build_budget(self):
        self.run_builder()
        seconds = int(self.parameters[0]['executionTimeout'][0])
        self.assertGreater(seconds, 600)
        self.assertLessEqual(seconds, self.config['maximumBuildSeconds'])
        receipt = json.loads((self.root / 'output/ami-build.json').read_text())
        self.assertEqual(receipt['state'], 'available')
        self.assertIn('sealAcknowledgedAt', receipt)
        self.assertTrue(receipt['builderTerminationRequested'])

    def test_base_with_extra_ebs_disk_is_rejected_before_launch(self):
        self.extra_base_disk = True
        with self.assertRaisesRegex(ValueError, 'single EBS root'):
            self.run_builder()
        self.assertNotIn('run-instances', [operation for _, operation, _ in self.calls])

    def test_builder_with_extra_attached_disk_is_never_imaged(self):
        self.extra_builder_disk = True
        with self.assertRaisesRegex(ValueError, 'single EBS root'):
            self.run_builder()
        self.assertNotIn('create-image', [operation for _, operation, _ in self.calls])
        receipt = json.loads((self.root / 'output/ami-build.json').read_text())
        self.assertTrue(receipt['builderTerminationRequested'])

    def test_stopped_without_seal_acknowledgement_is_never_imaged(self):
        self.seal_acknowledged = False
        with self.assertRaisesRegex(RuntimeError, 'seal acknowledgement'):
            self.run_builder()
        self.assertNotIn('create-image', [operation for _, operation, _ in self.calls])
        receipt = json.loads((self.root / 'output/ami-build.json').read_text())
        self.assertTrue(receipt['builderTerminationRequested'])

    def test_sigterm_uses_receipted_builder_cleanup(self):
        self.interrupt_readiness = True
        with self.assertRaisesRegex(InterruptedError, 'interrupted by signal'):
            self.run_builder()
        receipt = json.loads((self.root / 'output/ami-build.json').read_text())
        self.assertEqual(receipt['state'], 'failed')
        self.assertTrue(receipt['builderTerminationRequested'])

    def test_first_sigterm_during_cleanup_does_not_cancel_termination(self):
        self.interrupt_termination = True
        self.run_builder()
        receipt = json.loads((self.root / 'output/ami-build.json').read_text())
        self.assertEqual(receipt['state'], 'available')
        self.assertTrue(receipt['builderTerminationRequested'])


if __name__ == '__main__':
    unittest.main()
