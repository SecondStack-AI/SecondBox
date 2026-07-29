package main

import (
	"reflect"
	"testing"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestResolveCommandAliases(t *testing.T) {
	tests := []struct {
		args      []string
		operation string
		rest      []string
	}{
		{[]string{"profiles", "disable"}, "disableProfile", nil},
		{[]string{"runner-pools", "create"}, "createRunnerPool", nil},
		{[]string{"runner-pools", "update"}, "updateRunnerPool", nil},
		{[]string{"runners", "list"}, "listRunners", nil},
		{[]string{"runners", "get"}, "getRunner", nil},
		{[]string{"sandboxes", "restore"}, "restoreSandboxSnapshot", nil},
		{[]string{"exec"}, "executeSandboxCommand", nil},
		{[]string{"shell", "create"}, "createSandboxTerminal", nil},
		{[]string{"files", "read"}, "readSandboxFile", nil},
		{[]string{"snapshots", "create"}, "createSandboxSnapshot", nil},
		{[]string{"operation", "getSandbox", "--path", "sandboxId=sandbox-1"}, "getSandbox", []string{"--path", "sandboxId=sandbox-1"}},
	}
	for _, test := range tests {
		operation, rest, err := resolveCommand(test.args)
		if err != nil {
			t.Fatalf("resolveCommand(%v): %v", test.args, err)
		}
		if operation != test.operation || !reflect.DeepEqual(rest, test.rest) {
			t.Errorf("resolveCommand(%v) = %q, %v; want %q, %v", test.args, operation, rest, test.operation, test.rest)
		}
	}
}

func TestCommandAliasesReferenceGeneratedOperations(t *testing.T) {
	for command, alias := range commandAliases {
		if _, found := secondboxclient.LookupOperation(alias.operation); !found {
			t.Errorf("command %q references unknown operation %q", command, alias.operation)
		}
	}
}
