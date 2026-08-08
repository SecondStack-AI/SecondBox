package install

import (
	"strings"
	"testing"
)

func TestExistingWorkspaceMountRejectsRootDeviceReuse(t *testing.T) {
	plan := validPlan(t)
	plan.Storage = StoragePlan{Choice: StorageExistingMount, WorkspacePath: "/srv/secondbox/workspaces", ExistingDeviceIdentity: "8:1"}
	mountInfo := strings.Join([]string{
		"21 1 8:1 / / rw,relatime - btrfs /dev/vda1 rw",
		"22 21 8:1 /data /srv/secondbox rw,relatime - btrfs /dev/vda1 rw",
	}, "\n")
	if err := verifyExistingWorkspaceMountInfo(plan, []byte(mountInfo)); err == nil || !strings.Contains(err.Error(), "identity or filesystem changed") {
		t.Fatalf("root-device Workspace mount result = %v", err)
	}
	plan.Storage.ExistingDeviceIdentity = "8:2"
	mountInfo = strings.Replace(mountInfo, "22 21 8:1", "22 21 8:2", 1)
	if err := verifyExistingWorkspaceMountInfo(plan, []byte(mountInfo)); err != nil {
		t.Fatalf("dedicated Workspace mount result = %v", err)
	}
}
