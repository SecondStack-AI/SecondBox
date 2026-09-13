package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerGuestSchedulingWaitsForAllModes(t *testing.T) {
	driver := readRepositoryFile(t, "scripts/installer-qualification-driver")
	start := strings.Index(driver, "launch_guest() {")
	if start < 0 {
		t.Fatal("installer scheduler is absent")
	}
	end := strings.Index(driver[start:], "\nevidence_files=()")
	if end < 0 {
		t.Fatal("installer scheduler is absent")
	}
	schedule := driver[start : start+end]
	for _, tc := range []struct {
		name        string
		parallelism string
		modes       string
		fail        bool
	}{
		{"parallel", "3", "btrfs_image existing_reflink_filesystem existing_reflink_recreation", false}, {"serial", "1", "btrfs_image existing_reflink_filesystem existing_reflink_recreation", false}, {"failed_guest", "3", "btrfs_image existing_reflink_filesystem existing_reflink_recreation", true}, {"lean", "1", "btrfs_image", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			script := `set -Eeuo pipefail
cd "$1"
guest_parallelism="$2"
guest_jobs=()
read -ra selected_modes <<<"$3"
active_child=''
run_guest() {
  touch "started-$1"
  if [[ "$guest_parallelism" == 3 ]]; then
    for ((attempt=0; attempt<200; attempt++)); do
      count=$(find . -name 'started-*' | wc -l)
      ((count == 3)) && break
      sleep 0.01
    done
    ((count == 3)) || { echo 'guests did not overlap'; exit 1; }
  fi
  if [[ "$fail_guest" == true && "$1" == btrfs_image ]]; then return 17; fi
  sleep 0.05
  touch "finished-$1"
}
`
			// A failed first guest must not let the parent merge evidence or remove
			// the shared candidate before the other two guests finish.
			script += "\nfail_guest="
			if tc.fail {
				script += "true\n"
			} else {
				script += "false\n"
			}
			script += schedule + "\ntouch merged\n"
			command := exec.Command("bash", "-c", script, "scheduler-test", directory, tc.parallelism, tc.modes)
			output, err := command.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("scheduler result: %v\n%s", err, output)
			}
			for _, mode := range strings.Fields(tc.modes) {
				if tc.fail && mode == "btrfs_image" {
					continue
				}
				if _, err := os.Stat(filepath.Join(directory, "finished-"+mode)); err != nil {
					t.Fatalf("did not wait for %s: %v\n%s", mode, err, output)
				}
			}
			_, mergedErr := os.Stat(filepath.Join(directory, "merged"))
			if tc.fail && !os.IsNotExist(mergedErr) {
				t.Fatal("failed guest allowed merge")
			}
			if !tc.fail && mergedErr != nil {
				t.Fatal(mergedErr)
			}
		})
	}
}
