package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStatfsExistingAncestorUsesTheCreateOnlyTargetFilesystem(t *testing.T) {
	root := t.TempDir()
	want := unix.Statfs_t{}
	if err := unix.Statfs(root, &want); err != nil {
		t.Fatal(err)
	}
	got, err := statfsExistingAncestor(filepath.Join(root, "not-created", "runner-root"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != want.Type || got.Bsize != want.Bsize || got.Blocks != want.Blocks {
		t.Fatalf("ancestor statfs = %#v, want filesystem identity from %#v", got, want)
	}

	target := filepath.Join(root, "unsafe", "runner-root")
	if err := os.Symlink(t.TempDir(), filepath.Dir(target)); err != nil {
		t.Fatal(err)
	}
	if _, err := statfsExistingAncestor(target); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("symlink ancestor result = %v", err)
	}
}

func TestExistingWorkspaceMountRejectsRootDeviceReuse(t *testing.T) {
	plan := validPlan(t)
	plan.Storage.ExistingDeviceIdentity = "8:1"
	mountInfo := strings.Join([]string{
		"21 1 8:1 / / rw,relatime - btrfs /dev/vda1 rw",
		"22 21 8:1 /data /srv/secondbox rw,relatime - btrfs /dev/vda1 rw",
	}, "\n")
	if err := verifyExistingWorkspaceMountInfo(plan, []byte(mountInfo), true); err == nil || !strings.Contains(err.Error(), "identity or filesystem changed") {
		t.Fatalf("root-device Workspace mount result = %v", err)
	}
	plan.Storage.ExistingDeviceIdentity = "8:2"
	mountInfo = strings.Replace(mountInfo, "22 21 8:1", "22 21 8:2", 1)
	if err := verifyExistingWorkspaceMountInfo(plan, []byte(mountInfo), true); err != nil {
		t.Fatalf("dedicated Workspace mount result = %v", err)
	}
	for _, incompatible := range []string{"noexec", "nodev"} {
		incompatibleMount := strings.Replace(mountInfo, "/data /srv/secondbox rw,relatime - btrfs /dev/vda1 rw", "/data /srv/secondbox rw,relatime,"+incompatible+" - btrfs /dev/vda1 rw,"+incompatible, 1)
		if err := verifyExistingWorkspaceMountInfo(plan, []byte(incompatibleMount), true); err == nil || !strings.Contains(err.Error(), "must permit executable files and device nodes") {
			t.Fatalf("%s Runner storage mount result = %v", incompatible, err)
		}
	}
	plan.Storage.ExistingDeviceIdentity = "259:99"
	if err := verifyExistingWorkspaceMountInfo(plan, []byte(mountInfo), false); err != nil {
		t.Fatalf("completed host apply depended on transient device identity: %v", err)
	}
}
