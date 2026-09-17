package imagepreparation

import "testing"

func TestResolveTargetPrefersWidestArchitectureCoverage(t *testing.T) {
	targets := []Target{
		{RunnerID: "runner-a", Architectures: []string{"amd64"}},
		{RunnerID: "runner-b", Architectures: []string{"amd64", "arm64"}},
		{RunnerID: "runner-c", Architectures: []string{"arm64"}},
	}
	if chosen := ResolveTarget(targets); chosen.RunnerID != "runner-b" {
		t.Fatalf("resolve target = %q, want the widest architecture coverage", chosen.RunnerID)
	}
	if chosen := ResolveTarget(targets[:1]); chosen.RunnerID != "runner-a" {
		t.Fatalf("single captured target = %q", chosen.RunnerID)
	}
}
