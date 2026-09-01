package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const contractPath = "contracts/openapi/v1/secondbox.openapi.json"

var httpMethods = []string{"delete", "get", "patch", "post", "put"}

var goContractSchemas = stringSet([]string{
	"AcquireLeaseRequest",
	"BootStageTiming",
	"BootTiming",
	"CreateProfileRequest",
	"CreateRunnerPoolRequest",
	"CreateSandboxRequest",
	"CreateSnapshotRequest",
	"DeploymentTimingSummary",
	"DurationPercentiles",
	"ExecTiming",
	"ExecutionPolicy",
	"Lease",
	"LifecyclePolicy",
	"NetworkDestination",
	"NetworkPolicy",
	"Operation",
	"OperationStageTiming",
	"OperationTiming",
	"PortPolicy",
	"Problem",
	"Profile",
	"ProfileRevisionSpec",
	"RenewLeaseRequest",
	"ResourcePolicy",
	"RestoreSnapshotRequest",
	"RetentionPolicy",
	"Runner",
	"RunnerPage",
	"RunnerPool",
	"RunnerPoolPage",
	"Sandbox",
	"SandboxPage",
	"SandboxTiming",
	"Snapshot",
	"SnapshotPage",
	"StartupPolicy",
})

type document struct {
	Paths      map[string]json.RawMessage `json:"paths"`
	Components struct {
		PathItems map[string]json.RawMessage `json:"pathItems"`
		Schemas   map[string]schema          `json:"schemas"`
	} `json:"components"`
}

type schema struct {
	Ref                  string               `json:"$ref"`
	Type                 string               `json:"type"`
	Format               string               `json:"format"`
	Description          string               `json:"description"`
	Properties           map[string]schema    `json:"properties"`
	Required             []string             `json:"required"`
	Items                *schema              `json:"items"`
	Enum                 []json.RawMessage    `json:"enum"`
	Const                json.RawMessage      `json:"const"`
	OneOf                []schema             `json:"oneOf"`
	AdditionalProperties json.RawMessage      `json:"additionalProperties"`
	Discriminator        *schemaDiscriminator `json:"discriminator"`
}

type schemaDiscriminator struct {
	PropertyName string            `json:"propertyName"`
	Mapping      map[string]string `json:"mapping"`
}

type operation struct {
	ID                  string
	Method              string
	Path                string
	RequestBody         []operationMediaType
	RequestBodyRequired bool
}

type operationMediaType struct {
	ContentType string
	Schema      string
}

type openAPIOperation struct {
	OperationID string `json:"operationId"`
	RequestBody *struct {
		Required bool `json:"required"`
		Content  map[string]struct {
			Schema schema `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
}

func main() {
	outputRoot := flag.String("output-root", ".", "repository root receiving generated SDK files")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("SecondBox SDK generator does not accept positional arguments"))
	}

	contract, err := os.ReadFile(contractPath)
	if err != nil {
		fatal(fmt.Errorf("SecondBox SDK generator read contract: %w", err))
	}
	var document document
	if err := json.Unmarshal(contract, &document); err != nil {
		fatal(fmt.Errorf("SecondBox SDK generator decode contract: %w", err))
	}
	operations, err := collectOperations(document)
	if err != nil {
		fatal(err)
	}

	outputs := map[string][]byte{}
	if outputs["sdk/go/secondboxclient/transport_generated.go"], err = generateGoOperations(operations); err != nil {
		fatal(err)
	}
	if outputs["sdk/go/secondboxclient/wire_types_generated.go"], err = generateGoSchemas(document.Components.Schemas); err != nil {
		fatal(err)
	}
	if outputs["sdk/typescript/transport.generated.ts"], err = generateTypeScript(operations, document.Components.Schemas); err != nil {
		fatal(err)
	}
	if outputs["sdk/typescript/public-surface.json"], err = generateTypeScriptPublicSurface(document.Components.Schemas); err != nil {
		fatal(err)
	}
	for relativePath, contents := range outputs {
		path := filepath.Join(*outputRoot, relativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatal(fmt.Errorf("SecondBox SDK generator create %s directory: %w", relativePath, err))
		}
		if err := os.WriteFile(path, contents, 0o644); err != nil {
			fatal(fmt.Errorf("SecondBox SDK generator write %s: %w", relativePath, err))
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func collectOperations(document document) ([]operation, error) {
	paths := sortedKeys(document.Paths)
	operations := make([]operation, 0, len(paths))
	seen := make(map[string]bool)
	for _, path := range paths {
		pathItem, err := resolvePathItem(document, document.Paths[path])
		if err != nil {
			return nil, fmt.Errorf("SecondBox SDK generator resolve path %s: %w", path, err)
		}
		for _, method := range httpMethods {
			rawOperation, exists := pathItem[method]
			if !exists {
				continue
			}
			var decoded openAPIOperation
			if err := json.Unmarshal(rawOperation, &decoded); err != nil {
				return nil, fmt.Errorf("SecondBox SDK generator decode %s %s: %w", method, path, err)
			}
			if decoded.OperationID == "" {
				return nil, fmt.Errorf("SecondBox SDK generator %s %s has no operationId", method, path)
			}
			if seen[decoded.OperationID] {
				return nil, fmt.Errorf("SecondBox SDK generator duplicate operationId %q", decoded.OperationID)
			}
			seen[decoded.OperationID] = true
			generated := operation{ID: decoded.OperationID, Method: strings.ToUpper(method), Path: path}
			if decoded.RequestBody != nil {
				generated.RequestBodyRequired = decoded.RequestBody.Required
				for _, contentType := range sortedKeys(decoded.RequestBody.Content) {
					media := decoded.RequestBody.Content[contentType]
					schemaName, err := operationSchemaName(media.Schema)
					if err != nil {
						return nil, fmt.Errorf("SecondBox SDK generator %s request %s: %w", decoded.OperationID, contentType, err)
					}
					generated.RequestBody = append(generated.RequestBody, operationMediaType{
						ContentType: contentType,
						Schema:      schemaName,
					})
				}
			}
			operations = append(operations, generated)
		}
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i].ID < operations[j].ID })
	return operations, nil
}

func operationSchemaName(definition schema) (string, error) {
	if definition.Ref != "" {
		return referencedName(definition.Ref)
	}
	if definition.Type != "" {
		return definition.Type, nil
	}
	return "", errors.New("request body has no schema reference or type")
}

func resolvePathItem(document document, raw json.RawMessage) (map[string]json.RawMessage, error) {
	var pathItem map[string]json.RawMessage
	if err := json.Unmarshal(raw, &pathItem); err != nil {
		return nil, err
	}
	refValue, exists := pathItem["$ref"]
	if !exists {
		return pathItem, nil
	}
	var ref string
	if err := json.Unmarshal(refValue, &ref); err != nil {
		return nil, fmt.Errorf("decode path-item reference: %w", err)
	}
	const prefix = "#/components/pathItems/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("unsupported path-item reference %q", ref)
	}
	referenced, exists := document.Components.PathItems[strings.TrimPrefix(ref, prefix)]
	if !exists {
		return nil, fmt.Errorf("path-item reference %q does not exist", ref)
	}
	return resolvePathItem(document, referenced)
}

func generateGoOperations(operations []operation) ([]byte, error) {
	var output bytes.Buffer
	output.WriteString("// Code generated by secondbox-sdkgen. DO NOT EDIT.\n\n")
	output.WriteString("package secondboxclient\n\n")
	output.WriteString("var operations = map[string]OperationMetadata{\n")
	for _, operation := range operations {
		fmt.Fprintf(&output, "\t%q: {\n", operation.ID)
		fmt.Fprintf(&output, "\t\tOperationID: %q, Method: %q, PathTemplate: %q,\n", operation.ID, operation.Method, operation.Path)
		if len(operation.RequestBody) > 0 {
			output.WriteString("\t\tRequestBody: []OperationMediaType{\n")
			for _, media := range operation.RequestBody {
				fmt.Fprintf(&output, "\t\t\t{ContentType: %q, Schema: %q},\n", media.ContentType, media.Schema)
			}
			output.WriteString("\t\t},\n")
		}
		if operation.RequestBodyRequired {
			output.WriteString("\t\tRequestBodyRequired: true,\n")
		}
		output.WriteString("\t},\n")
	}
	output.WriteString("}\n\n")
	output.WriteString("// GetSandboxOperation is retained for callers that use the lower-level Do method.\n")
	output.WriteString("var GetSandboxOperation = operations[\"getSandbox\"]\n")
	formatted, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("SecondBox SDK generator format Go operations: %w", err)
	}
	return formatted, nil
}

func generateGoSchemas(schemas map[string]schema) ([]byte, error) {
	for _, name := range sortedKeys(goContractSchemas) {
		if _, exists := schemas[name]; !exists {
			return nil, fmt.Errorf("SecondBox SDK generator Go contract schema %s does not exist", name)
		}
	}
	var output bytes.Buffer
	output.WriteString("// Code generated by secondbox-sdkgen. DO NOT EDIT.\n\n")
	output.WriteString("package secondboxclient\n\n")
	output.WriteString("import (\n")
	output.WriteString("\t\"time\"\n\n")
	output.WriteString("\t\"github.com/SecondStack-AI/SecondBox/pkg/contracts\"\n")
	output.WriteString(")\n\n")
	for _, name := range sortedKeys(schemas) {
		definition := schemas[name]
		if definition.Description != "" {
			fmt.Fprintf(&output, "// %s %s\n", name, goComment(definition.Description))
		}
		if goContractSchemas[name] {
			fmt.Fprintf(&output, "type %s = contracts.%s\n\n", name, name)
			continue
		}
		if len(definition.Properties) > 0 || (definition.Type == "object" && len(definition.OneOf) > 0) {
			fmt.Fprintf(&output, "type %s struct {\n", name)
			required := stringSet(definition.Required)
			for _, propertyName := range sortedKeys(definition.Properties) {
				property := definition.Properties[propertyName]
				if property.Type == "integer" && property.Format == "" && goIntegerProperty(name, propertyName) {
					property.Format = "int"
				}
				propertyType, err := goSchemaType(property, schemas, !required[propertyName])
				if err != nil {
					return nil, fmt.Errorf("SecondBox SDK generator Go schema %s.%s: %w", name, propertyName, err)
				}
				tag := propertyName
				if !required[propertyName] {
					tag += ",omitempty"
				}
				fmt.Fprintf(&output, "\t%s %s `json:%s`\n", goIdentifier(propertyName), propertyType, strconv.Quote(tag))
			}
			output.WriteString("}\n\n")
			continue
		}
		if len(definition.OneOf) > 0 {
			fmt.Fprintf(&output, "type %s struct {\n", name)
			for _, variant := range definition.OneOf {
				variantName, err := referencedName(variant.Ref)
				if err != nil {
					return nil, fmt.Errorf("SecondBox SDK generator Go union %s: %w", name, err)
				}
				fmt.Fprintf(&output, "\t%s *%s `json:\"-\"`\n", variantName, variantName)
			}
			output.WriteString("}\n\n")
			continue
		}
		definitionType, err := goSchemaType(definition, schemas, false)
		if err != nil {
			return nil, fmt.Errorf("SecondBox SDK generator Go schema %s: %w", name, err)
		}
		fmt.Fprintf(&output, "type %s = %s\n", name, definitionType)
		if len(definition.Enum) > 0 {
			output.WriteString("\nconst (\n")
			for _, rawValue := range definition.Enum {
				var value string
				if err := json.Unmarshal(rawValue, &value); err != nil {
					return nil, fmt.Errorf("SecondBox SDK generator Go enum %s contains a non-string value", name)
				}
				fmt.Fprintf(&output, "\t%s%s %s = %q\n", name, goIdentifier(value), name, value)
			}
			output.WriteString(")\n")
		}
		output.WriteString("\n")
	}
	formatted, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("SecondBox SDK generator format Go schemas: %w", err)
	}
	return formatted, nil
}

func goSchemaType(definition schema, schemas map[string]schema, optional bool) (string, error) {
	if nullable, ok := nullableSchemaVariant(definition); ok {
		result, err := goSchemaType(nullable, schemas, false)
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(result, "*") {
			result = "*" + result
		}
		return result, nil
	}
	var result string
	if definition.Ref != "" {
		name, err := referencedName(definition.Ref)
		if err != nil {
			return "", err
		}
		if _, exists := schemas[name]; !exists {
			return "", fmt.Errorf("schema reference %q does not exist", definition.Ref)
		}
		result = name
		if optional && goSchemaNeedsPointer(schemas[name]) {
			result = "*" + result
		}
		return result, nil
	}
	if len(definition.Const) > 0 {
		result = goJSONLiteralType(definition.Const)
		if result == "" {
			return "", fmt.Errorf("unsupported const value %s", definition.Const)
		}
	} else if len(definition.Enum) > 0 {
		switch definition.Type {
		case "string":
			result = "string"
		case "integer":
			result = goIntegerType(definition.Format)
		default:
			return "", fmt.Errorf("unsupported enum type %q", definition.Type)
		}
	} else {
		switch definition.Type {
		case "string":
			if definition.Format == "date-time" {
				result = "time.Time"
			} else {
				result = "string"
			}
		case "integer":
			result = goIntegerType(definition.Format)
		case "number":
			result = "float64"
		case "boolean":
			result = "bool"
		case "array":
			if definition.Items == nil {
				return "", errors.New("array has no items schema")
			}
			itemType, err := goSchemaType(*definition.Items, schemas, false)
			if err != nil {
				return "", err
			}
			result = "[]" + itemType
		case "object":
			additional, present, err := decodeAdditionalProperties(definition.AdditionalProperties)
			if err != nil {
				return "", err
			}
			if !present {
				result = "map[string]any"
			} else {
				valueType, err := goSchemaType(additional, schemas, false)
				if err != nil {
					return "", err
				}
				result = "map[string]" + valueType
			}
		case "":
			result = "any"
		default:
			return "", fmt.Errorf("unsupported schema type %q", definition.Type)
		}
	}
	if optional && !strings.HasPrefix(result, "[]") && !strings.HasPrefix(result, "map[") && result != "any" {
		result = "*" + result
	}
	return result, nil
}

func goJSONLiteralType(value json.RawMessage) string {
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return ""
	}
	switch decoded.(type) {
	case string:
		return "string"
	case float64:
		return "float64"
	case bool:
		return "bool"
	default:
		return ""
	}
}

func goSchemaNeedsPointer(definition schema) bool {
	if definition.Type == "array" {
		return false
	}
	if definition.Type == "object" {
		_, present, _ := decodeAdditionalProperties(definition.AdditionalProperties)
		return !present
	}
	return true
}

func goIntegerType(format string) string {
	if format == "int" {
		return "int"
	}
	return "int64"
}

func goIntegerProperty(schemaName, propertyName string) bool {
	switch schemaName + "." + propertyName {
	case "CreateTerminalRequest.columns",
		"CreateTerminalRequest.rows",
		"ExecExited.exitCode",
		"ExecExited.signal",
		"Problem.status",
		"StreamSignalFrame.signal",
		"TerminalResizeFrame.columns",
		"TerminalResizeFrame.rows":
		return true
	default:
		return false
	}
}

func generateTypeScript(operations []operation, schemas map[string]schema) ([]byte, error) {
	var output bytes.Buffer
	output.WriteString("// Code generated by secondbox-sdkgen. DO NOT EDIT.\n\n")
	output.WriteString("export type JSONValue =\n")
	output.WriteString("  | null\n  | boolean\n  | number\n  | string\n  | readonly JSONValue[]\n")
	output.WriteString("  | { readonly [key: string]: JSONValue };\n\n")
	for _, name := range sortedKeys(schemas) {
		definition := schemas[name]
		if definition.Description != "" {
			fmt.Fprintf(&output, "/** %s */\n", typeScriptComment(definition.Description))
		}
		if len(definition.Properties) > 0 || (definition.Type == "object" && len(definition.OneOf) > 0) {
			fmt.Fprintf(&output, "export interface %s {\n", name)
			required := stringSet(definition.Required)
			for _, propertyName := range sortedKeys(definition.Properties) {
				propertyType, err := typeScriptSchemaType(definition.Properties[propertyName], schemas)
				if err != nil {
					return nil, fmt.Errorf("SecondBox SDK generator TypeScript schema %s.%s: %w", name, propertyName, err)
				}
				optional := "?"
				if required[propertyName] {
					optional = ""
				}
				fmt.Fprintf(&output, "  readonly %s%s: %s;\n", propertyName, optional, propertyType)
			}
			output.WriteString("}\n\n")
			continue
		}
		definitionType, err := typeScriptSchemaType(definition, schemas)
		if err != nil {
			return nil, fmt.Errorf("SecondBox SDK generator TypeScript schema %s: %w", name, err)
		}
		fmt.Fprintf(&output, "export type %s = %s;\n\n", name, definitionType)
	}
	output.WriteString("export type OperationID =\n")
	for index, operation := range operations {
		terminator := ""
		if index == len(operations)-1 {
			terminator = ";"
		}
		fmt.Fprintf(&output, "  | %q%s\n", operation.ID, terminator)
	}
	output.WriteString("\nexport type Route = {\n")
	output.WriteString("  readonly method: string;\n  readonly path: string;\n  readonly contentType?: string;\n};\n\n")
	output.WriteString("export const OPERATIONS: Readonly<Record<OperationID, Route>> = {\n")
	for _, operation := range operations {
		fmt.Fprintf(&output, "  %s: { method: %q, path: %q", operation.ID, operation.Method, operation.Path)
		if len(operation.RequestBody) > 1 {
			return nil, fmt.Errorf("SecondBox SDK generator TypeScript operation %s has multiple request content types", operation.ID)
		}
		if len(operation.RequestBody) == 1 {
			fmt.Fprintf(&output, ", contentType: %q", operation.RequestBody[0].ContentType)
		}
		output.WriteString(" },\n")
	}
	output.WriteString("};\n")
	return output.Bytes(), nil
}

type typeScriptSurface struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

func generateTypeScriptPublicSurface(schemas map[string]schema) ([]byte, error) {
	surface := make(map[string]typeScriptSurface)
	for _, name := range sortedKeys(schemas) {
		definition := schemas[name]
		if len(definition.Properties) == 0 && !(definition.Type == "object" && len(definition.OneOf) > 0) {
			continue
		}
		requiredSet := stringSet(definition.Required)
		shape := typeScriptSurface{}
		for _, propertyName := range sortedKeys(definition.Properties) {
			if requiredSet[propertyName] {
				shape.Required = append(shape.Required, propertyName)
			} else {
				shape.Optional = append(shape.Optional, propertyName)
			}
		}
		if shape.Required == nil {
			shape.Required = []string{}
		}
		if shape.Optional == nil {
			shape.Optional = []string{}
		}
		surface[name] = shape
	}
	surface["TransportRequestOptions"] = typeScriptSurface{
		Required: []string{},
		Optional: []string{"body", "contentType", "headers", "pathParameters", "queryParameters", "signal"},
	}
	encoded, err := json.MarshalIndent(surface, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("SecondBox SDK generator encode TypeScript public surface: %w", err)
	}
	return append(encoded, '\n'), nil
}

func typeScriptSchemaType(definition schema, schemas map[string]schema) (string, error) {
	if definition.Ref != "" {
		name, err := referencedName(definition.Ref)
		if err != nil {
			return "", err
		}
		if _, exists := schemas[name]; !exists {
			return "", fmt.Errorf("schema reference %q does not exist", definition.Ref)
		}
		return name, nil
	}
	if len(definition.Const) > 0 {
		return string(definition.Const), nil
	}
	if len(definition.OneOf) > 0 && len(definition.Properties) == 0 {
		variants := make([]string, 0, len(definition.OneOf))
		for _, variant := range definition.OneOf {
			variantType, err := typeScriptSchemaType(variant, schemas)
			if err != nil {
				return "", err
			}
			variants = append(variants, variantType)
		}
		return strings.Join(variants, " | "), nil
	}
	if len(definition.Enum) > 0 {
		values := make([]string, len(definition.Enum))
		for index, value := range definition.Enum {
			values[index] = string(value)
		}
		return strings.Join(values, " | "), nil
	}
	switch definition.Type {
	case "null":
		return "null", nil
	case "string":
		return "string", nil
	case "integer", "number":
		return "number", nil
	case "boolean":
		return "boolean", nil
	case "array":
		if definition.Items == nil {
			return "", errors.New("array has no items schema")
		}
		itemType, err := typeScriptSchemaType(*definition.Items, schemas)
		if err != nil {
			return "", err
		}
		return "readonly " + parenthesizeTypeScriptUnion(itemType) + "[]", nil
	case "object":
		additional, present, err := decodeAdditionalProperties(definition.AdditionalProperties)
		if err != nil {
			return "", err
		}
		if !present {
			return "Readonly<Record<string, JSONValue>>", nil
		}
		valueType, err := typeScriptSchemaType(additional, schemas)
		if err != nil {
			return "", err
		}
		return "Readonly<Record<string, " + valueType + ">>", nil
	case "":
		return "JSONValue", nil
	default:
		return "", fmt.Errorf("unsupported schema type %q", definition.Type)
	}
}

func nullableSchemaVariant(definition schema) (schema, bool) {
	if len(definition.OneOf) != 2 {
		return schema{}, false
	}
	if definition.OneOf[0].Type == "null" {
		return definition.OneOf[1], true
	}
	if definition.OneOf[1].Type == "null" {
		return definition.OneOf[0], true
	}
	return schema{}, false
}

func decodeAdditionalProperties(raw json.RawMessage) (schema, bool, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("false")) || bytes.Equal(raw, []byte("true")) {
		return schema{}, false, nil
	}
	var additional schema
	if err := json.Unmarshal(raw, &additional); err != nil {
		return schema{}, false, fmt.Errorf("decode additionalProperties: %w", err)
	}
	return additional, true, nil
}

func referencedName(ref string) (string, error) {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) || strings.TrimPrefix(ref, prefix) == "" {
		return "", fmt.Errorf("unsupported schema reference %q", ref)
	}
	return strings.TrimPrefix(ref, prefix), nil
}

func goIdentifier(value string) string {
	words := strings.FieldsFunc(value, func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
	})
	if len(words) == 0 {
		words = []string{value}
	}
	for index, word := range words {
		words[index] = strings.ToUpper(word[:1]) + word[1:]
	}
	identifier := strings.Join(words, "")
	for _, replacement := range []struct{ from, to string }{
		{"Id", "ID"}, {"Url", "URL"}, {"Uri", "URI"},
	} {
		if strings.HasSuffix(identifier, replacement.from) {
			identifier = strings.TrimSuffix(identifier, replacement.from) + replacement.to
		}
	}
	for _, replacement := range []struct{ from, to string }{
		{"Api", "API"}, {"Cpu", "CPU"}, {"Http", "HTTP"},
	} {
		if strings.HasPrefix(identifier, replacement.from) {
			identifier = replacement.to + strings.TrimPrefix(identifier, replacement.from)
		}
	}
	if identifier == "Cidr" {
		return "CIDR"
	}
	if identifier == "Sha256" {
		return "SHA256"
	}
	return identifier
}

func goComment(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func typeScriptComment(value string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(value), " "), "*/", "* /")
}

func parenthesizeTypeScriptUnion(value string) string {
	if strings.Contains(value, " | ") {
		return "(" + value + ")"
	}
	return value
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sortedKeys[Value any](values map[string]Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
