package store

import (
	"strings"
	"testing"

	"secondstack/sandbox-service/pkg/contracts"
)

func TestPostgresEnvironmentNaturalKeyIsUTF8SafeAndUnambiguous(t *testing.T) {
	first, err := postgresEnvironmentNaturalKey(contracts.Environment{
		TenantRef:      "tenant",
		SubjectRef:     "subject|environment",
		EnvironmentKey: "key",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := postgresEnvironmentNaturalKey(contracts.Environment{
		TenantRef:      "tenant",
		SubjectRef:     "subject",
		EnvironmentKey: "environment|key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(first, '\x00') || strings.ContainsRune(second, '\x00') {
		t.Fatalf("PostgreSQL advisory-lock keys contain a NUL byte: %q %q", first, second)
	}
	if first == second {
		t.Fatalf("distinct Environment natural keys collide: %q", first)
	}
}
