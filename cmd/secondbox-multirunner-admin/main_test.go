package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRequiresExplicitQualificationDatabase(t *testing.T) {
	t.Setenv("SECONDBOX_MULTIRUNNER_DATABASE_URL", "")

	err := run(context.Background(), []string{
		"request-runner-drain",
		"--runner-id", "runner-a",
		"--deadline-seconds", "300",
	})
	if err == nil || !strings.Contains(err.Error(), "SECONDBOX_MULTIRUNNER_DATABASE_URL") {
		t.Fatalf("expected explicit database error, got %v", err)
	}
}

func TestRunRejectsUnboundedDrainDeadline(t *testing.T) {
	t.Setenv("SECONDBOX_MULTIRUNNER_DATABASE_URL", "postgres://qualification")

	err := run(context.Background(), []string{
		"request-runner-drain",
		"--runner-id", "runner-a",
		"--deadline-seconds", "3601",
	})
	if err == nil || !strings.Contains(err.Error(), "from 1 through 3600") {
		t.Fatalf("expected bounded deadline error, got %v", err)
	}
}

func TestQualificationDrainMessageIDIsUniqueAndDiscoverable(t *testing.T) {
	first, err := qualificationDrainMessageID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := qualificationDrainMessageID()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "qualification-runner-drain-") {
		t.Fatalf("unexpected qualification drain identities %q and %q", first, second)
	}
}
