package main

import (
	"reflect"
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
