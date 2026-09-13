package deployment_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestScenarioShardSplitter(t *testing.T) {
	for _, test := range []struct {
		shard string
		want  string
	}{
		{"1/2", "^(TestAlpha|TestGamma)$"},
		{"2/2", "^(TestBeta)$"},
		{"1/1", "^(TestAlpha|TestBeta|TestGamma)$"},
		{"3/3", "^(TestGamma)$"},
		{"0/2", ""}, {"3/2", ""}, {"1/4", ""},
		{"1/0", ""}, {"01/2", ""}, {"1", ""},
		{"1/9999999999999999999999", ""},
	} {
		t.Run(test.shard, func(t *testing.T) {
			command := exec.Command("bash", "../../scripts/scenario-shard.sh", test.shard)
			command.Stdin = strings.NewReader("TestGamma\nBenchmarkIgnored\nTestAlpha\nok package 0.1s\nTestBeta\nTestAlpha\n")
			output, err := command.CombinedOutput()
			if test.want == "" {
				if err == nil {
					t.Fatalf("invalid shard accepted: %s", output)
				}
				return
			}
			if err != nil || strings.TrimSpace(string(output)) != test.want {
				t.Fatalf("split: %q, %v; want %q", output, err, test.want)
			}
		})
	}
}
