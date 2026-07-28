package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func generateGoClient(document openAPIDocument, operations []clientOperation, sourceHash string) []byte {
	var output bytes.Buffer
	output.WriteString(generatedHeader("//", sourceHash))
	output.WriteString(`
package secondboxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ContractVersion is the OpenAPI info.version represented by this generated transport.
const ContractVersion = ` + fmt.Sprintf("%q", document.Info.Version) + `

`)
	for _, schemaName := range sortedMapKeys(document.Components.Schemas) {
		writeGoSchema(&output, document, schemaName, document.Components.Schemas[schemaName])
	}
	writeGoOperationMetadata(&output, operations)
	writeGoTransport(&output)
	return output.Bytes()
}

func writeGoSchema(output *bytes.Buffer, document openAPIDocument, name string, value schema) {
	description := value.Description
	if description == "" {
		description = "is the wire representation of the " + splitIdentifierWords(name) + " schema."
	}
	fmt.Fprintf(output, "// %s %s\n", name, goDocSentence(description))

	if len(value.OneOf) > 0 && value.Type == "" {
		writeGoUnionSchema(output, document, name, value)
		return
	}
	if value.Type == "object" && len(value.Properties) > 0 {
		fmt.Fprintf(output, "type %s struct {\n", name)
		required := stringSet(value.Required)
		for _, propertyName := range sortedMapKeys(value.Properties) {
			property := value.Properties[propertyName]
			fieldName := goExportedIdentifier(propertyName)
			fieldType := goSchemaType(property)
			jsonTag := propertyName
			if _, isRequired := required[propertyName]; !isRequired {
				fieldType = "*" + fieldType
				jsonTag += ",omitempty"
			}
			fieldDescription := property.Description
			if fieldDescription == "" {
				fieldDescription = "carries the " + propertyName + " JSON field."
			}
			fmt.Fprintf(output, "\t// %s %s\n", fieldName, goDocSentence(fieldDescription))
			fmt.Fprintf(output, "\t%s %s `json:%q`\n", fieldName, fieldType, jsonTag)
		}
		output.WriteString("}\n\n")
		return
	}

	fmt.Fprintf(output, "type %s %s\n\n", name, goSchemaType(value))
	if len(value.Enum) == 0 {
		return
	}
	output.WriteString("const (\n")
	for _, enumValue := range value.Enum {
		constantName := name + goExportedIdentifier(enumValue)
		fmt.Fprintf(output, "\t// %s is the %q %s value.\n", constantName, enumValue, splitIdentifierWords(name))
		fmt.Fprintf(output, "\t%s %s = %q\n", constantName, name, enumValue)
	}
	output.WriteString(")\n\n")
}

func writeGoUnionSchema(output *bytes.Buffer, document openAPIDocument, name string, value schema) {
	variants := make([]string, 0, len(value.OneOf))
	for _, candidate := range value.OneOf {
		variants = append(variants, strings.TrimPrefix(candidate.Ref, "#/components/schemas/"))
	}
	fmt.Fprintf(output, "type %s struct {\n", name)
	for _, variant := range variants {
		fmt.Fprintf(output, "\t// %s contains the %s variant when selected.\n", variant, splitIdentifierWords(variant))
		fmt.Fprintf(output, "\t%s *%s `json:\"-\"`\n", variant, variant)
	}
	output.WriteString("}\n\n")

	fmt.Fprintf(output, "// MarshalJSON encodes exactly one selected %s variant.\n", name)
	fmt.Fprintf(output, "func (value %s) MarshalJSON() ([]byte, error) {\n", name)
	output.WriteString("\tselected := 0\n\tvar encoded []byte\n\tvar encodeErr error\n")
	for _, variant := range variants {
		fmt.Fprintf(output, "\tif value.%s != nil {\n\t\tselected++\n\t\tencoded, encodeErr = json.Marshal(value.%s)\n\t}\n", variant, variant)
	}
	fmt.Fprintf(output, "\tif encodeErr != nil {\n\t\treturn nil, fmt.Errorf(\"SecondBox %s encode variant: %%w\", encodeErr)\n\t}\n", name)
	fmt.Fprintf(output, "\tif selected != 1 {\n\t\treturn nil, fmt.Errorf(\"SecondBox %s requires exactly one variant, found %%d\", selected)\n\t}\n", name)
	output.WriteString("\treturn encoded, nil\n}\n\n")

	discriminatorProperty := ""
	if value.Discriminator != nil {
		discriminatorProperty = value.Discriminator.PropertyName
	}
	fmt.Fprintf(output, "// UnmarshalJSON decodes a %s by its %q discriminator.\n", name, discriminatorProperty)
	fmt.Fprintf(output, "func (value *%s) UnmarshalJSON(data []byte) error {\n", name)
	fmt.Fprintf(output, "\tvar discriminator struct {\n\t\tValue string `json:%q`\n\t}\n", discriminatorProperty)
	fmt.Fprintf(output, "\tif err := json.Unmarshal(data, &discriminator); err != nil {\n\t\treturn fmt.Errorf(\"SecondBox %s decode discriminator: %%w\", err)\n\t}\n", name)
	output.WriteString("\tswitch discriminator.Value {\n")
	for _, variant := range variants {
		discriminatorValue := goUnionDiscriminatorValue(document, value, variant)
		fmt.Fprintf(output, "\tcase %q:\n", discriminatorValue)
		fmt.Fprintf(output, "\t\tvar decoded %s\n", variant)
		fmt.Fprintf(output, "\t\tif err := json.Unmarshal(data, &decoded); err != nil {\n\t\t\treturn fmt.Errorf(\"SecondBox %s decode %s: %%w\", err)\n\t\t}\n", name, variant)
		fmt.Fprintf(output, "\t\t*value = %s{%s: &decoded}\n", name, variant)
		output.WriteString("\t\treturn nil\n")
	}
	fmt.Fprintf(output, "\tdefault:\n\t\treturn fmt.Errorf(\"SecondBox %s has unsupported discriminator %%q\", discriminator.Value)\n\t}\n", name)
	output.WriteString("}\n\n")
}

func goUnionDiscriminatorValue(document openAPIDocument, union schema, variantName string) string {
	if union.Discriminator != nil {
		for discriminatorValue, ref := range union.Discriminator.Mapping {
			if strings.TrimPrefix(ref, "#/components/schemas/") == variantName {
				return discriminatorValue
			}
		}
		if variant, ok := document.Components.Schemas[variantName]; ok {
			property := variant.Properties[union.Discriminator.PropertyName]
			if len(property.Const) > 0 {
				var value string
				if json.Unmarshal(property.Const, &value) == nil {
					return value
				}
			}
		}
	}
	return variantName
}

func goSchemaType(value schema) string {
	if value.Ref != "" {
		return strings.TrimPrefix(value.Ref, "#/components/schemas/")
	}
	if len(value.Const) > 0 {
		var stringConstant string
		if json.Unmarshal(value.Const, &stringConstant) == nil {
			return "string"
		}
		var boolConstant bool
		if json.Unmarshal(value.Const, &boolConstant) == nil {
			return "bool"
		}
		var integerConstant int64
		if json.Unmarshal(value.Const, &integerConstant) == nil {
			return "int64"
		}
	}
	switch value.Type {
	case "string":
		return "string"
	case "integer":
		if value.Format == "int64" {
			return "int64"
		}
		return "int"
	case "number":
		return "float64"
	case "boolean":
		return "bool"
	case "array":
		if value.Items == nil {
			return "[]json.RawMessage"
		}
		return "[]" + goSchemaType(*value.Items)
	case "object":
		if len(value.AdditionalProperties) > 0 && string(value.AdditionalProperties) != "false" {
			var additionalSchema schema
			if json.Unmarshal(value.AdditionalProperties, &additionalSchema) == nil && (additionalSchema.Type != "" || additionalSchema.Ref != "") {
				return "map[string]" + goSchemaType(additionalSchema)
			}
			return "map[string]json.RawMessage"
		}
		return "map[string]json.RawMessage"
	default:
		return "json.RawMessage"
	}
}

func writeGoOperationMetadata(output *bytes.Buffer, operations []clientOperation) {
	output.WriteString(`// OperationParameter describes one canonical path, query, or header parameter.
type OperationParameter struct {
	// Name is the parameter's exact wire name.
	Name string
	// Location is path, query, or header.
	Location string
	// Required reports whether the contract requires the parameter.
	Required bool
	// Schema is the component schema name or primitive wire type.
	Schema string
}

// OperationMediaType describes one request body representation.
type OperationMediaType struct {
	// ContentType is the exact HTTP media type.
	ContentType string
	// Schema is the component schema name or primitive wire type.
	Schema string
}

// OperationResponse describes one declared response representation.
type OperationResponse struct {
	// StatusCode is the OpenAPI response status or default.
	StatusCode string
	// ContentType is empty for responses without a body.
	ContentType string
	// Schema is empty for responses without a body.
	Schema string
}

// OperationMetadata is the canonical transport description for one OpenAPI operation.
type OperationMetadata struct {
	// OperationID is the stable OpenAPI operationId.
	OperationID string
	// Method is the uppercase HTTP method.
	Method string
	// PathTemplate is the versioned path with named placeholders.
	PathTemplate string
	// Parameters lists the operation's path, query, and header inputs.
	Parameters []OperationParameter
	// RequestBody lists every accepted request representation.
	RequestBody []OperationMediaType
	// RequestBodyRequired reports whether the operation requires a body.
	RequestBodyRequired bool
	// Responses lists every declared status and representation.
	Responses []OperationResponse
}

`)
	for _, operation := range operations {
		varName := goExportedIdentifier(operation.ID) + "Operation"
		fmt.Fprintf(output, "// %s describes the %s OpenAPI operation.\n", varName, operation.ID)
		fmt.Fprintf(output, "var %s = OperationMetadata{\n", varName)
		fmt.Fprintf(output, "\tOperationID: %q,\n\tMethod: %q,\n\tPathTemplate: %q,\n", operation.ID, operation.Method, operation.Path)
		if len(operation.Parameters) > 0 {
			output.WriteString("\tParameters: []OperationParameter{\n")
			for _, parameter := range operation.Parameters {
				fmt.Fprintf(output, "\t\t{Name: %q, Location: %q, Required: %t, Schema: %q},\n", parameter.Name, parameter.Location, parameter.Required, parameter.Schema)
			}
			output.WriteString("\t},\n")
		}
		if len(operation.RequestBody) > 0 {
			output.WriteString("\tRequestBody: []OperationMediaType{\n")
			for _, mediaType := range operation.RequestBody {
				fmt.Fprintf(output, "\t\t{ContentType: %q, Schema: %q},\n", mediaType.ContentType, mediaType.Schema)
			}
			output.WriteString("\t},\n")
		}
		fmt.Fprintf(output, "\tRequestBodyRequired: %t,\n", operation.RequestBodyRequired)
		if len(operation.Responses) > 0 {
			output.WriteString("\tResponses: []OperationResponse{\n")
			for _, response := range operation.Responses {
				fmt.Fprintf(output, "\t\t{StatusCode: %q, ContentType: %q, Schema: %q},\n", response.StatusCode, response.ContentType, response.Schema)
			}
			output.WriteString("\t},\n")
		}
		output.WriteString("}\n\n")
	}
	output.WriteString("// LookupOperation returns immutable-by-value metadata for a stable OpenAPI operationId.\n")
	output.WriteString("func LookupOperation(operationID string) (OperationMetadata, bool) {\n\tswitch operationID {\n")
	for _, operation := range operations {
		fmt.Fprintf(output, "\tcase %q:\n\t\treturn %sOperation, true\n", operation.ID, goExportedIdentifier(operation.ID))
	}
	output.WriteString("\tdefault:\n\t\treturn OperationMetadata{}, false\n\t}\n}\n\n")
}

func writeGoTransport(output *bytes.Buffer) {
	output.WriteString(`// RequestOptions supplies wire values for a generated operation.
type RequestOptions struct {
	// PathParameters replaces named placeholders in OperationMetadata.PathTemplate.
	PathParameters map[string]string
	// QueryParameters supplies already typed query values.
	QueryParameters url.Values
	// Headers supplies operation headers such as Idempotency-Key and If-Match.
	Headers http.Header
	// Body is the encoded request body, if any.
	Body io.Reader
	// ContentType selects one declared request media type.
	ContentType string
}

// APIError is a non-successful SecondBox HTTP response with its structured problem when available.
type APIError struct {
	// StatusCode is the HTTP response status.
	StatusCode int
	// Problem is the RFC 9457-compatible SecondBox problem body when decoding succeeded.
	Problem *Problem
	// Body contains the bounded raw error body.
	Body []byte
}

// Error returns a greppable SecondBox transport error.
func (failure *APIError) Error() string {
	if failure.Problem != nil {
		return fmt.Sprintf("SecondBox API request failed: status=%d code=%s title=%s", failure.StatusCode, failure.Problem.Code, failure.Problem.Title)
	}
	return fmt.Sprintf("SecondBox API request failed: status=%d", failure.StatusCode)
}

// Client is the dependency-free HTTP transport for generated SecondBox operations.
type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

// NewSecondBoxClient validates transport dependencies without inventing lifecycle behavior.
func NewSecondBoxClient(rawURL, token string, httpClient *http.Client) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("SecondBox client URL must be an absolute HTTP endpoint without query or fragment")
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, errors.New("SecondBox client URL scheme must be http or https")
	}
	if token == "" {
		return nil, errors.New("SecondBox client service-account token is required")
	}
	if httpClient == nil {
		return nil, errors.New("SecondBox client HTTP client is required")
	}
	return &Client{baseURL: baseURL, token: token, httpClient: httpClient}, nil
}

// Do sends one generated operation and leaves successful response decoding to the typed caller.
func (client *Client) Do(ctx context.Context, operation OperationMetadata, options RequestOptions) (*http.Response, error) {
	path := operation.PathTemplate
	for _, parameter := range operation.Parameters {
		if parameter.Location != "path" {
			continue
		}
		value, exists := options.PathParameters[parameter.Name]
		if parameter.Required && (!exists || value == "") {
			return nil, fmt.Errorf("SecondBox client missing required path parameter %q for %s", parameter.Name, operation.OperationID)
		}
		path = strings.ReplaceAll(path, "{"+parameter.Name+"}", url.PathEscape(value))
	}
	if strings.Contains(path, "{") {
		return nil, fmt.Errorf("SecondBox client has unresolved path template %q for %s", path, operation.OperationID)
	}
	endpoint := client.baseURL.ResolveReference(&url.URL{Path: path})
	endpoint.RawQuery = options.QueryParameters.Encode()

	contentType := options.ContentType
	if options.Body != nil && contentType == "" && len(operation.RequestBody) == 1 {
		contentType = operation.RequestBody[0].ContentType
	}
	if contentType != "" && !operationAcceptsContentType(operation, contentType) {
		return nil, fmt.Errorf("SecondBox client content type %q is not declared for %s", contentType, operation.OperationID)
	}
	request, err := http.NewRequestWithContext(ctx, operation.Method, endpoint.String(), options.Body)
	if err != nil {
		return nil, fmt.Errorf("SecondBox client create %s request: %w", operation.OperationID, err)
	}
	request.Header = options.Headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("SecondBox client send %s request: %w", operation.OperationID, err)
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return response, nil
	}
	defer response.Body.Close()
	const maximumProblemBytes = 4 << 20
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumProblemBytes+1))
	if readErr != nil {
		return nil, fmt.Errorf("SecondBox client read %s error response: %w", operation.OperationID, readErr)
	}
	if len(body) > maximumProblemBytes {
		return nil, fmt.Errorf("SecondBox client %s error response exceeds %d bytes", operation.OperationID, maximumProblemBytes)
	}
	failure := &APIError{StatusCode: response.StatusCode, Body: body}
	var problem Problem
	if json.Unmarshal(body, &problem) == nil {
		failure.Problem = &problem
	}
	return nil, failure
}

func operationAcceptsContentType(operation OperationMetadata, contentType string) bool {
	for _, representation := range operation.RequestBody {
		if representation.ContentType == contentType {
			return true
		}
	}
	return false
}

// EncodeJSONBody serializes a strongly typed generated request for RequestOptions.Body.
func EncodeJSONBody(value interface{}) (io.Reader, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("SecondBox client encode JSON request: %w", err)
	}
	return bytes.NewReader(encoded), nil
}
`)
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func goDocSentence(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.TrimSpace(value)
	if value == "" {
		return "is defined by the canonical SecondBox contract."
	}
	if !strings.HasSuffix(value, ".") {
		value += "."
	}
	return value
}

func splitIdentifierWords(value string) string {
	words := identifierWords(value)
	for index := range words {
		words[index] = strings.ToLower(words[index])
	}
	return strings.Join(words, " ")
}
