package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestComposeUpArgumentsRemoveOrphanedTopology(t *testing.T) {
	base := []string{"compose", "--project-name", "secondbox"}
	got := composeUpArguments(base, "--detach")
	want := []string{"compose", "--project-name", "secondbox", "up", "--remove-orphans", "--detach"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Compose up arguments = %#v", got)
	}
	if !reflect.DeepEqual(base, []string{"compose", "--project-name", "secondbox"}) {
		t.Fatalf("Compose base arguments mutated = %#v", base)
	}
}

func TestDockerComposeReceivesOperatorClientConfiguration(t *testing.T) {
	directory := t.TempDir()
	outputPath := filepath.Join(directory, "environment")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n' \"$DOCKER_CONFIG\" \"$SSH_AUTH_SOCK\" > " + strconv.Quote(outputPath) + "\n"
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("DOCKER_CONFIG", "/operator/docker-config")
	t.Setenv("SSH_AUTH_SOCK", "/operator/ssh-agent.sock")
	if err := runDockerCompose([]string{"compose", "version"}); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(output)); got != "/operator/docker-config\n/operator/ssh-agent.sock" {
		t.Fatalf("Docker client environment = %q", got)
	}
}
