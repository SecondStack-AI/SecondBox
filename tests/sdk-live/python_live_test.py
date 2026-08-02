import os
import uuid

from secondbox_client import SecondBoxAPIError, SecondBoxClient


client = SecondBoxClient(
    os.environ["SECONDBOX_LIVE_BASE_URL"],
    os.environ["SECONDBOX_LIVE_PLATFORM_TOKEN"],
    "sdk-live-python",
    "sdk-live-python-subject",
)
profile_name = "python-sdk-live"
profile = client.request_json(
    "createProfile",
    headers={"Idempotency-Key": f"python-profile-{uuid.uuid4()}"},
    body={
        "name": profile_name,
        "spec": {
            "pool": "compose-live-pool",
            "architecture": "amd64",
            "runtimeBundleDigest": "sha256:" + "a" * 64,
            "toolchainBundleDigest": "sha256:" + "b" * 64,
            "resources": {
                "cpuMillis": 1000,
                "memoryBytes": 2**30,
                "workspaceBytes": 8 * 2**30,
                "processLimit": 128,
                "concurrentOperations": 4,
            },
            "lifecycle": {
                "initialState": "stopped",
                "drainGraceSeconds": 30,
                "idleSeconds": 300,
                "maximumDurationSeconds": 3600,
                "leaseSeconds": 60,
            },
            "retention": {
                "snapshotLimit": 8,
                "snapshotRetentionSeconds": 86400,
                "artifactRetentionSeconds": 86400,
            },
            "execution": {
                "maximumDeadlineMilliseconds": 60000,
                "maximumBufferedOutputBytes": 2**20,
                "streamWindowBytes": 65536,
                "maximumTransferBytes": 2**30,
                "terminalDetachSeconds": 30,
            },
            "network": {"mode": "deny_all", "destinations": []},
            "ports": [],
        },
    },
)
operation = client.create_sandbox(
    profile["name"], {"sdk": "python"}, f"python-sandbox-{uuid.uuid4()}"
)
sandbox = client.get_sandbox(operation["sandboxId"])
assert sandbox["metadata"]["sdk"] == "python"
try:
    client.get_sandbox("sbx_missing_live_contract")
except SecondBoxAPIError as error:
    assert error.status == 404 and error.problem["code"] == "not_found"
else:
    raise AssertionError("missing sandbox did not return a structured API error")
