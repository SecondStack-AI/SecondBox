package deployment_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScenarioShardEvidenceMerge(t *testing.T) {
	script, err := filepath.Abs("../../scripts/scenario-merge-evidence.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range []string{"", "commit", "dirty", "missing", "duplicate", "count", "tier"} {
		t.Run("reject_"+problem, func(t *testing.T) {
			root := t.TempDir()
			run := func(args ...string) string {
				t.Helper()
				c := exec.Command(args[0], args[1:]...)
				c.Dir = root
				out, err := c.CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			run("git", "init", "-q")
			run("git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "fixture")
			commit := run("git", "rev-parse", "HEAD")
			dir := filepath.Join(root, ".git", "shards")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 2; i++ {
				source, dirty, test, tier, count := commit, "false", fmt.Sprintf("Test%d", i), "release", 1
				if i == 2 {
					switch problem {
					case "commit":
						source = strings.Repeat("a", 40)
					case "dirty":
						dirty = "true"
					case "missing":
						continue
					case "duplicate":
						test = "Test1"
					case "tier":
						tier = "nightly"
					case "count":
						count = 2
					}
				}
				file := filepath.Join(dir, fmt.Sprintf("scenario-shard-%d-evidence.json", i))
				evidence := fmt.Sprintf(`{"schemaVersion":"secondbox.release/qualification-evidence/v2","sourceCommit":%q,"repositoryDirty":%s,"suite":"test-scenario","passCount":%d,"wallClockSeconds":%d,"host":{"platform":"linux-amd64"},"qualifiedAt":"2026-09-13T00:00:00Z"}`, source, dirty, count, i*10)
				if err := os.WriteFile(file, []byte(evidence), 0600); err != nil {
					t.Fatal(err)
				}
				results := fmt.Sprintf(`{"shard":"%d/2","tier":%q,"tests":[{"test":%q,"result":"PASS"}]}`, i, tier, test)
				if err := os.WriteFile(file+".results.json", []byte(results), 0600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(dir, "merged.json")
			command := exec.Command("bash", script, "2", dir, output)
			command.Dir = root
			out, err := command.CombinedOutput()
			if (err != nil) != (problem != "") {
				t.Fatalf("merge: %v: %s", err, out)
			}
			if problem == "" {
				run("jq", "-e", ".passCount == 2 and .wallClockSeconds == 20", output)
			} else if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("failed merge left evidence")
			}
		})
	}
}
