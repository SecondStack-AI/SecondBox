package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const secondBoxOpenAPIPath = "contracts/openapi/v1/secondbox.openapi.json"

type openAPIDocument struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]pathItem `json:"paths"`
	Components struct {
		Schemas    map[string]schema      `json:"schemas"`
		Parameters map[string]parameter   `json:"parameters"`
		Responses  map[string]apiResponse `json:"responses"`
		PathItems  map[string]pathItem    `json:"pathItems"`
	} `json:"components"`
}

type schema struct {
	Ref                  string            `json:"$ref"`
	Type                 string            `json:"type"`
	Format               string            `json:"format"`
	Description          string            `json:"description"`
	Enum                 []string          `json:"enum"`
	Const                json.RawMessage   `json:"const"`
	Properties           map[string]schema `json:"properties"`
	Required             []string          `json:"required"`
	Items                *schema           `json:"items"`
	AdditionalProperties json.RawMessage   `json:"additionalProperties"`
	OneOf                []schema          `json:"oneOf"`
	Discriminator        *discriminator    `json:"discriminator"`
}

type discriminator struct {
	PropertyName string            `json:"propertyName"`
	Mapping      map[string]string `json:"mapping"`
}

type pathItem struct {
	Ref        string      `json:"$ref"`
	Parameters []parameter `json:"parameters"`
	Get        *operation  `json:"get"`
	Post       *operation  `json:"post"`
	Put        *operation  `json:"put"`
	Patch      *operation  `json:"patch"`
	Delete     *operation  `json:"delete"`
}

type operation struct {
	OperationID string                 `json:"operationId"`
	Summary     string                 `json:"summary"`
	Parameters  []parameter            `json:"parameters"`
	RequestBody *requestBody           `json:"requestBody"`
	Responses   map[string]apiResponse `json:"responses"`
}

type parameter struct {
	Ref         string `json:"$ref"`
	Name        string `json:"name"`
	In          string `json:"in"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
	Schema      schema `json:"schema"`
}

type requestBody struct {
	Required bool                 `json:"required"`
	Content  map[string]mediaType `json:"content"`
}

type apiResponse struct {
	Ref         string                 `json:"$ref"`
	Description string                 `json:"description"`
	Content     map[string]mediaType   `json:"content"`
	Headers     map[string]interface{} `json:"headers"`
}

type mediaType struct {
	Schema schema `json:"schema"`
}

type clientOperation struct {
	ID                  string
	Summary             string
	Method              string
	Path                string
	Parameters          []clientParameter
	RequestBody         []clientMediaType
	RequestBodyRequired bool
	Responses           []clientResponse
}

type clientParameter struct {
	Name        string
	Location    string
	Required    bool
	Description string
	Schema      string
}

type clientMediaType struct {
	ContentType string
	Schema      string
}

type clientResponse struct {
	StatusCode  string
	ContentType string
	Schema      string
}

type generatedOutput struct {
	Path    string
	Content []byte
}

func main() {
	check := flag.Bool("check", false, "fail when generated clients differ from the canonical OpenAPI contract")
	flag.Parse()
	if err := runClientGeneration(*check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runClientGeneration(check bool) error {
	specBytes, err := os.ReadFile(secondBoxOpenAPIPath)
	if err != nil {
		return fmt.Errorf("SecondBox client generation read contract: %w", err)
	}
	var document openAPIDocument
	if err := json.Unmarshal(specBytes, &document); err != nil {
		return fmt.Errorf("SecondBox client generation decode contract: %w", err)
	}
	operations, err := collectClientOperations(document)
	if err != nil {
		return err
	}
	if err := validateClientContract(document, operations, specBytes); err != nil {
		return err
	}

	sum := sha256.Sum256(specBytes)
	sourceHash := hex.EncodeToString(sum[:])
	outputs := []generatedOutput{
		{Path: "sdk/go/secondboxclient/client.gen.go", Content: generateGoClient(document, operations, sourceHash)},
		{Path: "sdk/typescript/secondbox-client.gen.ts", Content: generateTypeScriptClient(document, operations, sourceHash)},
		{Path: "sdk/python/secondbox_client_gen.py", Content: generatePythonClient(document, operations, sourceHash)},
	}
	outputs[0].Content, err = format.Source(outputs[0].Content)
	if err != nil {
		return fmt.Errorf("SecondBox client generation format Go output: %w", err)
	}

	for _, output := range outputs {
		content := append(bytes.TrimSpace(output.Content), '\n')
		if check {
			current, readErr := os.ReadFile(output.Path)
			if readErr != nil {
				return fmt.Errorf("SecondBox generated client read %s: %w", output.Path, readErr)
			}
			if !bytes.Equal(current, content) {
				return fmt.Errorf("SecondBox generated client %s is stale; run scripts/generate-clients.sh", output.Path)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(output.Path), 0o755); err != nil {
			return fmt.Errorf("SecondBox client generation create output directory: %w", err)
		}
		if err := os.WriteFile(output.Path, content, 0o644); err != nil {
			return fmt.Errorf("SecondBox client generation write %s: %w", output.Path, err)
		}
	}
	return nil
}

func validateClientContract(document openAPIDocument, operations []clientOperation, source []byte) error {
	if document.OpenAPI != "3.1.0" {
		return fmt.Errorf("SecondBox client generation requires OpenAPI 3.1.0, found %q", document.OpenAPI)
	}
	if document.Info.Title != "SecondBox API" || document.Info.Version == "" {
		return errors.New("SecondBox client generation requires the canonical SecondBox API title and a version")
	}
	if len(document.Components.Schemas) == 0 || len(operations) == 0 {
		return errors.New("SecondBox client generation requires public schemas and operations")
	}
	for _, forbidden := range []string{
		`"/v1/environments`,
		`"Environment"`,
		`"tenantRef"`,
		`"subjectRef"`,
		`"resourceClassId"`,
		`"lifecyclePolicyId"`,
		`"backendRef"`,
		`"hostPath"`,
		`SecondStack`,
	} {
		if bytes.Contains(source, []byte(forbidden)) {
			return fmt.Errorf("SecondBox client generation refuses legacy or private public-contract token %q", forbidden)
		}
	}
	seenOperationIDs := make(map[string]struct{}, len(operations))
	for _, candidate := range operations {
		if candidate.ID == "" {
			return fmt.Errorf("SecondBox client generation found %s %s without operationId", candidate.Method, candidate.Path)
		}
		if _, exists := seenOperationIDs[candidate.ID]; exists {
			return fmt.Errorf("SecondBox client generation found duplicate operationId %q", candidate.ID)
		}
		seenOperationIDs[candidate.ID] = struct{}{}
	}
	return nil
}

func collectClientOperations(document openAPIDocument) ([]clientOperation, error) {
	paths := sortedMapKeys(document.Paths)
	operations := make([]clientOperation, 0, len(paths))
	for _, path := range paths {
		item := document.Paths[path]
		if item.Ref != "" {
			name, err := componentReferenceName(item.Ref, "pathItems")
			if err != nil {
				return nil, err
			}
			resolved, ok := document.Components.PathItems[name]
			if !ok {
				return nil, fmt.Errorf("SecondBox client generation cannot resolve path item %q", item.Ref)
			}
			item = resolved
		}
		methods := []struct {
			name      string
			operation *operation
		}{
			{name: "GET", operation: item.Get},
			{name: "POST", operation: item.Post},
			{name: "PUT", operation: item.Put},
			{name: "PATCH", operation: item.Patch},
			{name: "DELETE", operation: item.Delete},
		}
		for _, method := range methods {
			if method.operation == nil {
				continue
			}
			converted, err := convertClientOperation(document, path, method.name, item.Parameters, *method.operation)
			if err != nil {
				return nil, err
			}
			operations = append(operations, converted)
		}
	}
	sort.Slice(operations, func(i, j int) bool {
		return operations[i].ID < operations[j].ID
	})
	return operations, nil
}

func convertClientOperation(document openAPIDocument, path, method string, pathParameters []parameter, source operation) (clientOperation, error) {
	result := clientOperation{
		ID:                  source.OperationID,
		Summary:             source.Summary,
		Method:              method,
		Path:                path,
		RequestBodyRequired: source.RequestBody != nil && source.RequestBody.Required,
	}
	for _, unresolved := range append(append([]parameter{}, pathParameters...), source.Parameters...) {
		resolved, err := resolveClientParameter(document, unresolved)
		if err != nil {
			return clientOperation{}, err
		}
		result.Parameters = append(result.Parameters, clientParameter{
			Name:        resolved.Name,
			Location:    resolved.In,
			Required:    resolved.Required,
			Description: resolved.Description,
			Schema:      clientSchemaReference(resolved.Schema),
		})
	}
	sort.Slice(result.Parameters, func(i, j int) bool {
		if result.Parameters[i].Location != result.Parameters[j].Location {
			return result.Parameters[i].Location < result.Parameters[j].Location
		}
		return result.Parameters[i].Name < result.Parameters[j].Name
	})

	if source.RequestBody != nil {
		for _, contentType := range sortedMapKeys(source.RequestBody.Content) {
			result.RequestBody = append(result.RequestBody, clientMediaType{
				ContentType: contentType,
				Schema:      clientSchemaReference(source.RequestBody.Content[contentType].Schema),
			})
		}
	}
	for _, statusCode := range sortedMapKeys(source.Responses) {
		response := source.Responses[statusCode]
		if response.Ref != "" {
			name, err := componentReferenceName(response.Ref, "responses")
			if err != nil {
				return clientOperation{}, err
			}
			var ok bool
			response, ok = document.Components.Responses[name]
			if !ok {
				return clientOperation{}, fmt.Errorf("SecondBox client generation cannot resolve response %q", response.Ref)
			}
		}
		if len(response.Content) == 0 {
			result.Responses = append(result.Responses, clientResponse{StatusCode: statusCode})
			continue
		}
		for _, contentType := range sortedMapKeys(response.Content) {
			result.Responses = append(result.Responses, clientResponse{
				StatusCode:  statusCode,
				ContentType: contentType,
				Schema:      clientSchemaReference(response.Content[contentType].Schema),
			})
		}
	}
	return result, nil
}

func resolveClientParameter(document openAPIDocument, unresolved parameter) (parameter, error) {
	if unresolved.Ref == "" {
		return unresolved, nil
	}
	name, err := componentReferenceName(unresolved.Ref, "parameters")
	if err != nil {
		return parameter{}, err
	}
	resolved, ok := document.Components.Parameters[name]
	if !ok {
		return parameter{}, fmt.Errorf("SecondBox client generation cannot resolve parameter %q", unresolved.Ref)
	}
	return resolved, nil
}

func componentReferenceName(ref, componentKind string) (string, error) {
	prefix := "#/components/" + componentKind + "/"
	if !strings.HasPrefix(ref, prefix) || strings.TrimPrefix(ref, prefix) == "" {
		return "", fmt.Errorf("SecondBox client generation unsupported component reference %q", ref)
	}
	return strings.TrimPrefix(ref, prefix), nil
}

func clientSchemaReference(value schema) string {
	if value.Ref != "" {
		return strings.TrimPrefix(value.Ref, "#/components/schemas/")
	}
	if value.Type == "string" && value.Format == "binary" {
		return "binary"
	}
	if value.Type != "" {
		return value.Type
	}
	return ""
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func generatedHeader(comment, sourceHash string) string {
	return fmt.Sprintf("%s Code generated from %s (sha256 %s); DO NOT EDIT.\n", comment, secondBoxOpenAPIPath, sourceHash)
}
