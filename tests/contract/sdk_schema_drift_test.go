package contract_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// schemaBackedTypes maps each canonical OpenAPI schema to the Go type that
// serializes it.
//
// These types are hand-maintained rather than generated, so nothing otherwise
// prevents a property from being added to the document and never reaching a
// serializer, or the reverse. Publishing streamWindowBytes required editing the
// contract, the document, and two SDKs by hand; this test is what makes a
// forgotten edit fail rather than ship.
var schemaBackedTypes = map[string]any{
	"Sandbox":           contracts.Sandbox{},
	"Operation":         contracts.Operation{},
	"Lease":             contracts.Lease{},
	"Snapshot":          contracts.Snapshot{},
	"Artifact":          contracts.Artifact{},
	"PortSession":       contracts.PortSession{},
	"TerminalSession":   contracts.TerminalSession{},
	"ExecStreamSession": contracts.ExecStreamSession{},
	"FileStat":          contracts.FileStat{},
	"Problem":           contracts.Problem{},
}

// duplicatedSDKTypes are Go SDK structs that restate a contracts type instead of
// aliasing it. An alias cannot drift; these can, so they are compared directly.
var duplicatedSDKTypes = map[string][2]any{
	"TerminalSession":   {secondboxclient.TerminalSession{}, contracts.TerminalSession{}},
	"ExecStreamSession": {secondboxclient.ExecStreamSession{}, contracts.ExecStreamSession{}},
	"FileStat":          {secondboxclient.FileStat{}, contracts.FileStat{}},
}

func canonicalSchemaProperties(t *testing.T, document openAPIDocument, name string) []string {
	t.Helper()
	components, _ := document["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	schema, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("canonical contract has no schema %q", name)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema %q declares no properties", name)
	}
	names := make([]string, 0, len(properties))
	for property := range properties {
		names = append(names, property)
	}
	sort.Strings(names)
	return names
}

func jsonFieldNames(value any) []string {
	structType := reflect.TypeOf(value)
	names := make([]string, 0, structType.NumField())
	for index := range structType.NumField() {
		tag := structType.Field(index).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		names = append(names, strings.Split(tag, ",")[0])
	}
	sort.Strings(names)
	return names
}

// TestSerializedTypesMirrorCanonicalSchemas fails when a schema property has no
// Go field, or a Go field has no schema property.
func TestSerializedTypesMirrorCanonicalSchemas(t *testing.T) {
	document := loadOpenAPIContract(t)
	for schemaName, value := range schemaBackedTypes {
		t.Run(schemaName, func(t *testing.T) {
			expected := canonicalSchemaProperties(t, document, schemaName)
			actual := jsonFieldNames(value)
			if !reflect.DeepEqual(expected, actual) {
				t.Errorf(
					"%s does not mirror its canonical schema\n  schema: %v\n  Go:     %v\n  missing from Go: %v\n  absent from schema: %v",
					schemaName, expected, actual,
					absentFrom(expected, actual), absentFrom(actual, expected),
				)
			}
		})
	}
}

// TestDuplicatedSDKTypesMatchTheirContract pins the SDK structs that restate a
// contracts type rather than aliasing it.
func TestDuplicatedSDKTypesMatchTheirContract(t *testing.T) {
	for name, pair := range duplicatedSDKTypes {
		t.Run(name, func(t *testing.T) {
			sdkFields, contractFields := jsonFieldNames(pair[0]), jsonFieldNames(pair[1])
			if !reflect.DeepEqual(sdkFields, contractFields) {
				t.Errorf(
					"Go SDK %s restates the contract type and has drifted\n  SDK:      %v\n  contract: %v",
					name, sdkFields, contractFields,
				)
			}
		})
	}
}

func absentFrom(want []string, have []string) []string {
	present := make(map[string]bool, len(have))
	for _, name := range have {
		present[name] = true
	}
	var absent []string
	for _, name := range want {
		if !present[name] {
			absent = append(absent, name)
		}
	}
	return absent
}
