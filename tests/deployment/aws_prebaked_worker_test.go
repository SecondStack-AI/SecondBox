package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrebakedWorkerCacheRequiresExactBundleAndImages(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*testing.T, string, string)
		fail   bool
	}{
		{name: "matching immutable cache"},
		{name: "missing bundle identity", fail: true, change: func(t *testing.T, cache, _ string) {
			if err := os.Remove(filepath.Join(cache, "prebaked.bundle.sha256")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "different bundle", fail: true, change: func(t *testing.T, cache, _ string) {
			writePrebakedFixture(t, filepath.Join(cache, "prebaked.bundle.sha256"), strings.Repeat("f", 64), 0o600)
		}},
		{name: "execution cache on the AMI root", fail: true, change: func(t *testing.T, cache, _ string) {
			if err := os.Mkdir(filepath.Join(cache, "execution-images"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "bundle archive on the AMI root", fail: true, change: func(t *testing.T, cache, _ string) {
			writePrebakedFixture(t, filepath.Join(cache, "worker.tar.gz"), "fixture", 0o600)
		}},
		{name: "mutable image reference", fail: true, change: func(t *testing.T, cache, _ string) {
			writePrebakedFixture(t, filepath.Join(cache, "runtime.env"), "SECONDBOX_RUNNER_IMAGE=runner:latest\nAGENT_RUNNER_GATEWAY_IMAGE=sha256:"+strings.Repeat("b", 64)+"\n", 0o600)
		}},
		{name: "missing Docker image", fail: true, change: func(t *testing.T, _, docker string) {
			writePrebakedFixture(t, docker, "#!/bin/sh\nexit 1\n", 0o755)
		}},
		{name: "different Docker image", fail: true, change: func(t *testing.T, _, docker string) {
			writePrebakedFixture(t, docker, "#!/bin/sh\nprintf 'sha256:wrong\\n'\n", 0o755)
		}},
		{name: "worker identity in cache", fail: true, change: func(t *testing.T, cache, _ string) {
			writePrebakedFixture(t, filepath.Join(cache, "runner.key"), "must-never-be-baked", 0o600)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			cache := t.TempDir()
			for _, directory := range []string{"bin", "artifacts/rootfs", "tools"} {
				if err := os.MkdirAll(filepath.Join(cache, directory), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"install-worker.sh", "stage-worker-execution-cache.sh", "bin/secondbox-fleet-enroll", "bin/secondbox-flat-root-digest"} {
				writePrebakedFixture(t, filepath.Join(cache, name), "#!/bin/sh\nexit 0\n", 0o755)
			}
			for _, name := range []string{"compose.yaml", "enrollment-ca.pem", "artifacts/materialization.json"} {
				writePrebakedFixture(t, filepath.Join(cache, name), "fixture", 0o600)
			}
			writePrebakedFixture(t, filepath.Join(cache, "runtime.env"), "SECONDBOX_RUNNER_IMAGE=sha256:"+strings.Repeat("a", 64)+"\nAGENT_RUNNER_GATEWAY_IMAGE=sha256:"+strings.Repeat("b", 64)+"\n", 0o600)
			writePrebakedFixture(t, filepath.Join(cache, "prebaked.bundle.sha256"), strings.Repeat("c", 64)+"\n", 0o600)
			docker := filepath.Join(cache, "tools/docker")
			writePrebakedFixture(t, docker, "#!/usr/bin/env bash\nset -eu\n[[ $1 == image && $2 == inspect ]] || exit 91\nprintf '%s\\n' \"${@: -1}\"\n", 0o755)
			if scenario.change != nil {
				scenario.change(t, cache, docker)
			}
			command := exec.Command("bash", filepath.Join(repositoryRootForDeploymentPolicy(t), "deploy/aws-fleet/verify-prebaked-worker.sh"), cache, strings.Repeat("c", 64))
			command.Env = append(os.Environ(), "PATH="+filepath.Dir(docker)+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := command.CombinedOutput()
			if (err != nil) != scenario.fail {
				t.Fatalf("cache result error=%v, want failure=%v: %s", err, scenario.fail, output)
			}
		})
	}
}

func writePrebakedFixture(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

// Verification records name bundle directories by their Runner container
// path, so the paths a worker image build records under must be the paths the
// worker's Runner uses.
func TestPrebakedWorkerRecordsVerificationsUnderRunnerPaths(t *testing.T) {
	repository := repositoryRootForDeploymentPolicy(t)
	compose, err := os.ReadFile(filepath.Join(repository, "deploy/aws-fleet/compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	preparer, err := os.ReadFile(filepath.Join(repository, "deploy/aws-fleet/prepare-prebaked-worker.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []struct{ variable, path string }{
		{"SECONDBOX_RUNNER_EXECUTION_IMAGE_CACHE_ROOT", "/var/lib/secondbox/state/execution-images"},
		{"SECONDBOX_RUNNER_EXECUTION_IMAGE_VERIFICATION_ROOT", "/var/lib/secondbox/state/execution-image-verifications"},
	} {
		if !strings.Contains(string(compose), setting.variable+": "+setting.path+"\n") {
			t.Fatalf("worker compose.yaml no longer sets %s to %s", setting.variable, setting.path)
		}
		if !strings.Contains(string(preparer), "--env "+setting.variable+"="+setting.path+" ") {
			t.Fatalf("pre-baked preparation does not record under %s=%s", setting.variable, setting.path)
		}
	}
	if !strings.Contains(string(compose), "- /srv/secondbox:/var/lib/secondbox:") || !strings.Contains(string(preparer), `--volume "$data:/var/lib/secondbox"`) {
		t.Fatal("pre-baked preparation and the worker Runner mount the data volume at different paths")
	}
}
