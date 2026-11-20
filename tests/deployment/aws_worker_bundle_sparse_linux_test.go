package deployment_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestAWSWorkerBundlePreservesSparseExecutionDisks(t *testing.T) {
	repository := repositoryRootForDeploymentPolicy(t)
	if err := os.MkdirAll(filepath.Join(repository, ".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.MkdirTemp(filepath.Join(repository, ".tmp"), "worker-sparse-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(fixture); err != nil {
			t.Error(err)
		}
	})
	for _, name := range []string{"tools", "artifacts/rootfs", "cache", "restored"} {
		if err := os.MkdirAll(filepath.Join(fixture, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writePrebakedFixture(t, filepath.Join(fixture, "artifacts/materialization.json"), "fixture", 0o600)
	writePrebakedFixture(t, filepath.Join(fixture, "ca.pem"), "public fixture CA", 0o600)
	writePrebakedFixture(t, filepath.Join(fixture, "enroll"), "#!/bin/sh\nprintf 'fixture enrollment version\\n'\n", 0o755)
	// Only external image/build commands and the privilege guard are replaced.
	// The real bundle entrypoint runs GNU cp/tar and its actual archive roundtrip.
	writePrebakedFixture(t, filepath.Join(fixture, "tools/id"), "#!/bin/sh\nprintf '0\\n'\n", 0o755)
	writePrebakedFixture(t, filepath.Join(fixture, "tools/go"), `#!/bin/bash
set -eu
while (($#)); do
  if [[ $1 == -o ]]; then
    printf '#!/bin/sh\nexit 0\n' > "$2"
    chmod 755 "$2"
    exit 0
  fi
  shift
done
exit 90
`, 0o755)
	writePrebakedFixture(t, filepath.Join(fixture, "tools/docker"), `#!/bin/bash
set -eu
case "$1 $2" in
  'image inspect') printf 'sha256:%064d\n' 1 ;;
  'image save') printf 'fixture Docker image archive' > "$4" ;;
  *) exit 91 ;;
esac
`, 0o755)
	content := make([]byte, 16*1024*1024)
	copy(content, "execution disk header")
	copy(content[len(content)-4096:], "execution disk tail")
	original := filepath.Join(fixture, "cache/rootfs.ext4")
	if err := os.WriteFile(original, content, 0o600); err != nil {
		t.Fatal(err)
	}
	modified := time.Unix(1700000000, 123456789)
	if err := os.Chtimes(original, modified, modified); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(fixture, "bundle")
	command := exec.Command("bash", filepath.Join(repository, "deploy/aws-fleet/build-worker-bundle.sh"),
		"fixture-runner", "fixture-gateway", filepath.Join(fixture, "artifacts"),
		filepath.Join(fixture, "ca.pem"), filepath.Join(fixture, "enroll"), output, filepath.Join(fixture, "cache"))
	command.Env = append(os.Environ(), "PATH="+filepath.Join(fixture, "tools")+":"+os.Getenv("PATH"))
	if result, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build real worker bundle: %v\n%s", err, result)
	}
	extract := exec.Command("tar", "--extract", "--gzip", "--same-permissions", "--file", output+".tar.gz",
		"--directory", filepath.Join(fixture, "restored"), "execution-images")
	if result, err := extract.CombinedOutput(); err != nil {
		t.Fatalf("extract real worker bundle: %v\n%s", err, result)
	}
	restored := filepath.Join(fixture, "restored/execution-images/rootfs.ext4")
	actual, err := os.ReadFile(restored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, content) || sha256.Sum256(actual) != sha256.Sum256(content) {
		t.Fatal("execution disk bytes changed across worker bundle")
	}
	info, err := os.Stat(restored)
	if err != nil {
		t.Fatal(err)
	}
	allocated := info.Sys().(*syscall.Stat_t).Blocks * 512
	if allocated >= info.Size()/4 {
		t.Fatalf("worker bundle expanded zero regions: logical=%d allocated=%d", info.Size(), allocated)
	}
	if info.Mode().Perm() != 0o600 || !info.ModTime().Equal(modified) {
		t.Fatalf("execution disk metadata changed: mode=%v mtime=%v", info.Mode(), info.ModTime())
	}
}

func TestAWSPrebakedExecutionCacheRetryKeepsLiveLocks(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	source := filepath.Join(root, "archive/execution-images")
	state := filepath.Join(root, "state")
	cache := filepath.Join(state, "execution-images")
	for _, directory := range []string{bundle, filepath.Join(source, ".locks"), filepath.Join(cache, ".locks")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writePrebakedFixture(t, filepath.Join(source, ".locks/digest.lock"), "", 0o600)
	writePrebakedFixture(t, filepath.Join(source, "existing.img"), "release bytes", 0o600)
	writePrebakedFixture(t, filepath.Join(source, "new.img"), "new image", 0o600)
	writePrebakedFixture(t, filepath.Join(cache, "existing.img"), "Runner-owned bytes", 0o600)
	writePrebakedFixture(t, filepath.Join(bundle, "prebaked.execution-images"), "archive\n", 0o600)
	if err := os.Chmod(cache, 0o710); err != nil {
		t.Fatal(err)
	}
	archive := exec.Command("tar", "--create", "--gzip", "--format=pax", "--file", filepath.Join(bundle, "worker.tar.gz"),
		"--directory", filepath.Join(root, "archive"), "execution-images")
	if output, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("create worker cache fixture: %v\n%s", err, output)
	}
	lockPath := filepath.Join(cache, ".locks/digest.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		stage := exec.Command("bash", filepath.Join(repositoryRootForDeploymentPolicy(t), "deploy/aws-fleet/stage-worker-execution-cache.sh"), bundle, state, "prebaked")
		if output, err := stage.CombinedOutput(); err != nil {
			t.Fatalf("stage retry %d: %v\n%s", attempt, err, output)
		}
		contender, err := os.OpenFile(lockPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		lockErr := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		contender.Close()
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
			t.Fatalf("staging replaced a live lock inode: contender result %v", lockErr)
		}
	}
	for name, expected := range map[string]string{"existing.img": "Runner-owned bytes", "new.img": "new image"} {
		actual, err := os.ReadFile(filepath.Join(cache, name))
		if err != nil || string(actual) != expected {
			t.Fatalf("cache %s: bytes=%q error=%v", name, actual, err)
		}
	}
	info, err := os.Stat(cache)
	if err != nil || info.Mode().Perm() != 0o710 {
		t.Fatalf("staging changed cache directory ownership mode: info=%v error=%v", info, err)
	}
}
