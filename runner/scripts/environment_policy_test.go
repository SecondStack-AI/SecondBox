package scripts_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerScriptsDoNotDefaultSecondBoxEnvironmentVariables(t *testing.T) {
	runnerRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := "$" + "{SECONDBOX_" + "RUNNER_" // Keep the policy token assembled so the test cannot exempt itself.
	err = filepath.WalkDir(runnerRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(path, ".sh"),
			strings.HasSuffix(path, ".service"),
			filepath.Base(path) == "Dockerfile",
			filepath.Base(path) == "Justfile":
		default:
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, line := range strings.Split(string(data), "\n") {
			start := strings.Index(line, forbidden)
			if start < 0 {
				continue
			}
			end := strings.Index(line[start:], "}")
			if end >= 0 && strings.Contains(line[start:start+end], ":-") {
				t.Errorf("%s defaults a SecondBox runner environment variable: %s", path, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
