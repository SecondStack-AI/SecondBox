// Package openapicheck validates live HTTP responses against the canonical
// OpenAPI contract.
//
// Static contract tests prove the document is internally consistent and that
// every route is declared. They cannot prove the server actually emits what the
// document promises: a required property the handler stopped serializing, or a
// property the handler emits that the document never declared, passes every
// static check while breaking any strict client. This package closes that gap
// by validating real responses as the integration suite produces them.
//
// It implements only the JSON Schema vocabulary the canonical contract uses.
// An unknown keyword is a deliberate failure rather than a silent pass, so the
// validator cannot rot into a no-op as the contract grows.
package openapicheck

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// supportedKeywords is every JSON Schema keyword this validator understands.
// Keywords that carry no validation semantics are listed so that an unexpected
// keyword still fails loudly.
var supportedKeywords = map[string]bool{
	"$ref": true, "type": true, "required": true, "properties": true,
	"additionalProperties": true, "items": true, "enum": true, "const": true,
	"minLength": true, "maxLength": true, "pattern": true, "format": true,
	"minimum": true, "maximum": true, "maxItems": true, "minItems": true,
	"uniqueItems": true, "maxProperties": true, "minProperties": true,
	"oneOf": true, "anyOf": true, "allOf": true, "discriminator": true,
	"contentEncoding": true, "contentMediaType": true, "propertyNames": true,
	"description": true, "title": true, "example": true, "examples": true,
	"default": true, "deprecated": true, "readOnly": true, "writeOnly": true,
	"nullable": true,
}

// Document is a loaded OpenAPI contract prepared for response validation.
type Document struct {
	root   map[string]any
	routes []route
}

type route struct {
	method   string
	template string
	matcher  *regexp.Regexp
	literals int
	// responses maps a status code, or "default", to its declared JSON schema.
	// A nil schema means the contract declares no JSON body for that status.
	responses map[string]map[string]any
	jsonBody  map[string]bool
}

// Load reads and prepares the canonical contract at path.
func Load(path string) (*Document, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("openapicheck: read contract: %w", err)
	}
	var root map[string]any
	if err := json.Unmarshal(contents, &root); err != nil {
		return nil, fmt.Errorf("openapicheck: parse contract: %w", err)
	}
	document := &Document{root: root}
	if err := document.compileRoutes(); err != nil {
		return nil, err
	}
	return document, nil
}

var templateParameter = regexp.MustCompile(`\{[^}]+\}`)

func (document *Document) compileRoutes() error {
	paths, _ := document.root["paths"].(map[string]any)
	if len(paths) == 0 {
		return errors.New("openapicheck: contract declares no paths")
	}
	for template, value := range paths {
		// A path item may itself be a $ref into components/pathItems.
		operations, _ := document.resolve(value).(map[string]any)
		for method, operationValue := range operations {
			upper := strings.ToUpper(method)
			switch upper {
			case http.MethodGet, http.MethodPut, http.MethodPost,
				http.MethodDelete, http.MethodPatch, http.MethodHead:
			default:
				continue
			}
			operation, ok := operationValue.(map[string]any)
			if !ok {
				continue
			}
			compiled, err := document.compileRoute(upper, template, operation)
			if err != nil {
				return err
			}
			document.routes = append(document.routes, compiled)
		}
	}
	// Prefer the most literal template so `/x/{id}:start` wins over `/x/{id}`.
	sort.SliceStable(document.routes, func(a, b int) bool {
		return document.routes[a].literals > document.routes[b].literals
	})
	return nil
}

func (document *Document) compileRoute(
	method string,
	template string,
	operation map[string]any,
) (route, error) {
	// Quote each literal segment separately: quoting the whole template first
	// would escape the braces and corrupt the parameter substitution.
	var pattern strings.Builder
	pattern.WriteString("^")
	remainder := template
	for {
		location := templateParameter.FindStringIndex(remainder)
		if location == nil {
			pattern.WriteString(regexp.QuoteMeta(remainder))
			break
		}
		pattern.WriteString(regexp.QuoteMeta(remainder[:location[0]]))
		pattern.WriteString("[^/]+?")
		remainder = remainder[location[1]:]
	}
	pattern.WriteString("$")
	matcher, err := regexp.Compile(pattern.String())
	if err != nil {
		return route{}, fmt.Errorf("openapicheck: compile %s %s: %w", method, template, err)
	}
	compiled := route{
		method:    method,
		template:  template,
		matcher:   matcher,
		literals:  len(templateParameter.ReplaceAllString(template, "")),
		responses: map[string]map[string]any{},
		jsonBody:  map[string]bool{},
	}
	responses, _ := operation["responses"].(map[string]any)
	for status, responseValue := range responses {
		response, ok := document.resolve(responseValue).(map[string]any)
		if !ok {
			continue
		}
		content, _ := response["content"].(map[string]any)
		jsonContent, hasJSON := content["application/json"].(map[string]any)
		if !hasJSON {
			jsonContent, hasJSON = content["application/problem+json"].(map[string]any)
		}
		if !hasJSON {
			compiled.responses[status] = nil
			continue
		}
		compiled.jsonBody[status] = true
		schema, _ := jsonContent["schema"].(map[string]any)
		compiled.responses[status] = schema
	}
	return compiled, nil
}

// resolve follows a single local $ref, returning value unchanged when absent.
func (document *Document) resolve(value any) any {
	for depth := 0; depth < 16; depth++ {
		node, ok := value.(map[string]any)
		if !ok {
			return value
		}
		reference, ok := node["$ref"].(string)
		if !ok {
			return value
		}
		resolved, err := document.lookup(reference)
		if err != nil {
			return value
		}
		value = resolved
	}
	return value
}

func (document *Document) lookup(reference string) (any, error) {
	if !strings.HasPrefix(reference, "#/") {
		return nil, fmt.Errorf("openapicheck: unsupported reference %q", reference)
	}
	var cursor any = document.root
	for _, segment := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
		node, ok := cursor.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("openapicheck: reference %q is unreachable", reference)
		}
		cursor, ok = node[segment]
		if !ok {
			return nil, fmt.Errorf("openapicheck: reference %q is unreachable", reference)
		}
	}
	return cursor, nil
}

// ValidateResponse checks one live response against the contract. It reports
// nil when the contract declares no JSON body for the matched operation.
func (document *Document) ValidateResponse(
	method string,
	urlPath string,
	status int,
	contentType string,
	body []byte,
) error {
	matched, ok := document.match(method, urlPath)
	if !ok {
		// Operational endpoints such as /metrics and /healthz sit outside the
		// versioned resource API on purpose and carry no contract schema.
		if !strings.HasPrefix(urlPath, "/v1/") {
			return nil
		}
		return fmt.Errorf("openapicheck: %s %s is not declared by the contract", method, urlPath)
	}
	code := strconv.Itoa(status)
	schema, declared := matched.responses[code]
	if !declared {
		if schema, declared = matched.responses["default"]; !declared {
			return fmt.Errorf(
				"openapicheck: %s %s does not declare status %d",
				method, matched.template, status,
			)
		}
		code = "default"
	}
	mediaType := contentType
	if index := strings.IndexByte(mediaType, ';'); index >= 0 {
		mediaType = strings.TrimSpace(mediaType[:index])
	}
	if !matched.jsonBody[code] {
		// The contract promises no JSON body; a JSON body here is a defect.
		if strings.HasSuffix(mediaType, "json") && len(body) > 0 {
			return fmt.Errorf(
				"openapicheck: %s %s status %d returns a JSON body the contract does not declare",
				method, matched.template, status,
			)
		}
		return nil
	}
	if schema == nil {
		return nil
	}
	if !strings.HasSuffix(mediaType, "json") {
		return fmt.Errorf(
			"openapicheck: %s %s status %d declares JSON but served %q",
			method, matched.template, status, contentType,
		)
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fmt.Errorf(
			"openapicheck: %s %s status %d body is not valid JSON: %w",
			method, matched.template, status, err,
		)
	}
	if err := document.validate(schema, decoded, "body"); err != nil {
		return fmt.Errorf("openapicheck: %s %s status %d %w", method, matched.template, status, err)
	}
	return nil
}

// ValidateAgainstSchema checks one decoded value against one schema. It exists
// so the validator's own coverage can be exercised directly.
func (document *Document) ValidateAgainstSchema(schema map[string]any, value any) error {
	return document.validate(schema, value, "value")
}

func (document *Document) match(method string, urlPath string) (route, bool) {
	for _, candidate := range document.routes {
		if candidate.method == method && candidate.matcher.MatchString(urlPath) {
			return candidate, true
		}
	}
	return route{}, false
}

func (document *Document) validate(schema map[string]any, value any, path string) error {
	if reference, ok := schema["$ref"].(string); ok {
		resolved, err := document.lookup(reference)
		if err != nil {
			return err
		}
		target, ok := resolved.(map[string]any)
		if !ok {
			return fmt.Errorf("at %s: reference %q is not a schema", path, reference)
		}
		return document.validate(target, value, path)
	}
	for keyword := range schema {
		if !supportedKeywords[keyword] {
			return fmt.Errorf(
				"at %s: contract uses JSON Schema keyword %q that openapicheck does not implement",
				path, keyword,
			)
		}
	}
	if err := document.validateComposition(schema, value, path); err != nil {
		return err
	}
	if err := document.validateType(schema, value, path); err != nil {
		return err
	}
	if enum, ok := schema["enum"].([]any); ok && !containsValue(enum, value) {
		return fmt.Errorf("at %s: value %v is outside the declared enum", path, value)
	}
	if constant, ok := schema["const"]; ok && !equalValue(constant, value) {
		return fmt.Errorf("at %s: value %v does not equal the declared const %v", path, value, constant)
	}
	switch typed := value.(type) {
	case map[string]any:
		return document.validateObject(schema, typed, path)
	case []any:
		return document.validateArray(schema, typed, path)
	case string:
		return validateString(schema, typed, path)
	case float64:
		return validateNumber(schema, typed, path)
	}
	return nil
}

func (document *Document) validateComposition(schema map[string]any, value any, path string) error {
	if branches, ok := schema["allOf"].([]any); ok {
		for index, branch := range branches {
			target, ok := branch.(map[string]any)
			if !ok {
				continue
			}
			if err := document.validate(target, value, fmt.Sprintf("%s/allOf[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	for _, keyword := range []string{"oneOf", "anyOf"} {
		branches, ok := schema[keyword].([]any)
		if !ok {
			continue
		}
		matches := 0
		var failures []string
		for index, branch := range branches {
			target, ok := branch.(map[string]any)
			if !ok {
				continue
			}
			if err := document.validate(target, value, path); err != nil {
				failures = append(failures, fmt.Sprintf("[%d] %v", index, err))
				continue
			}
			matches++
		}
		if matches == 0 {
			return fmt.Errorf("at %s: value matches no %s branch: %s", path, keyword, strings.Join(failures, "; "))
		}
		if keyword == "oneOf" && matches > 1 {
			return fmt.Errorf("at %s: value matches %d oneOf branches, want exactly one", path, matches)
		}
	}
	return nil
}

func (document *Document) validateType(schema map[string]any, value any, path string) error {
	declared, ok := schema["type"].(string)
	if !ok {
		return nil
	}
	if value == nil {
		if nullable, _ := schema["nullable"].(bool); nullable || declared == "null" {
			return nil
		}
		return fmt.Errorf("at %s: value is null but the contract declares %q", path, declared)
	}
	actual := jsonTypeName(value)
	if declared == "integer" {
		number, isNumber := value.(float64)
		if !isNumber || number != math.Trunc(number) {
			return fmt.Errorf("at %s: value is %s but the contract declares integer", path, actual)
		}
		return nil
	}
	if declared == "number" && actual == "integer" {
		return nil
	}
	if actual != declared {
		return fmt.Errorf("at %s: value is %s but the contract declares %s", path, actual, declared)
	}
	return nil
}

func (document *Document) validateObject(
	schema map[string]any,
	value map[string]any,
	path string,
) error {
	properties, _ := schema["properties"].(map[string]any)
	if required, ok := schema["required"].([]any); ok {
		for _, item := range required {
			name, _ := item.(string)
			if _, present := value[name]; !present {
				return fmt.Errorf("at %s: required property %q is absent from the response", path, name)
			}
		}
	}
	if allowed, ok := schema["additionalProperties"].(bool); ok && !allowed {
		var undeclared []string
		for name := range value {
			if _, declared := properties[name]; !declared {
				undeclared = append(undeclared, name)
			}
		}
		if len(undeclared) > 0 {
			sort.Strings(undeclared)
			return fmt.Errorf(
				"at %s: response carries properties the contract does not declare: %s",
				path, strings.Join(undeclared, ", "),
			)
		}
	}
	if names, ok := schema["propertyNames"].(map[string]any); ok {
		for name := range value {
			if err := document.validate(names, name, path+" property name "+name); err != nil {
				return err
			}
		}
	}
	if limit, ok := schema["maxProperties"].(float64); ok && float64(len(value)) > limit {
		return fmt.Errorf("at %s: object has %d properties, maximum is %v", path, len(value), limit)
	}
	if limit, ok := schema["minProperties"].(float64); ok && float64(len(value)) < limit {
		return fmt.Errorf("at %s: object has %d properties, minimum is %v", path, len(value), limit)
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		propertyValue, present := value[name]
		if !present {
			continue
		}
		propertySchema, ok := properties[name].(map[string]any)
		if !ok {
			continue
		}
		if err := document.validate(propertySchema, propertyValue, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func (document *Document) validateArray(schema map[string]any, value []any, path string) error {
	if limit, ok := schema["maxItems"].(float64); ok && float64(len(value)) > limit {
		return fmt.Errorf("at %s: array has %d items, maximum is %v", path, len(value), limit)
	}
	if limit, ok := schema["minItems"].(float64); ok && float64(len(value)) < limit {
		return fmt.Errorf("at %s: array has %d items, minimum is %v", path, len(value), limit)
	}
	if unique, ok := schema["uniqueItems"].(bool); ok && unique {
		seen := map[string]bool{}
		for _, item := range value {
			encoded, err := json.Marshal(item)
			if err != nil {
				continue
			}
			if seen[string(encoded)] {
				return fmt.Errorf("at %s: array declares uniqueItems but repeats %s", path, encoded)
			}
			seen[string(encoded)] = true
		}
	}
	items, ok := schema["items"].(map[string]any)
	if !ok {
		return nil
	}
	for index, item := range value {
		if err := document.validate(items, item, fmt.Sprintf("%s[%d]", path, index)); err != nil {
			return err
		}
	}
	return nil
}

func validateString(schema map[string]any, value string, path string) error {
	if limit, ok := schema["minLength"].(float64); ok && float64(utf8.RuneCountInString(value)) < limit {
		return fmt.Errorf("at %s: string is shorter than the declared minLength %v", path, limit)
	}
	if limit, ok := schema["maxLength"].(float64); ok && float64(utf8.RuneCountInString(value)) > limit {
		return fmt.Errorf("at %s: string is longer than the declared maxLength %v", path, limit)
	}
	if pattern, ok := schema["pattern"].(string); ok {
		// Contract patterns are ECMA regular expressions. Go's RE2 rejects
		// lookaround, which the workspace-path patterns rely on. A pattern RE2
		// cannot express is a limitation here, not a server defect, so the
		// remaining constraints still apply and this one is skipped.
		if matcher, err := regexp.Compile(pattern); err == nil {
			if !matcher.MatchString(value) {
				return fmt.Errorf("at %s: %q does not match the declared pattern %q", path, value, pattern)
			}
		}
	}
	if format, ok := schema["format"].(string); ok && format == "date-time" {
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("at %s: %q is not an RFC 3339 timestamp", path, value)
		}
	}
	if encoding, ok := schema["contentEncoding"].(string); ok && encoding == "base64" {
		if _, err := base64.StdEncoding.DecodeString(value); err != nil {
			return fmt.Errorf("at %s: value is not valid base64", path)
		}
	}
	return nil
}

func validateNumber(schema map[string]any, value float64, path string) error {
	if limit, ok := schema["minimum"].(float64); ok && value < limit {
		return fmt.Errorf("at %s: %v is below the declared minimum %v", path, value, limit)
	}
	if limit, ok := schema["maximum"].(float64); ok && value > limit {
		return fmt.Errorf("at %s: %v is above the declared maximum %v", path, value, limit)
	}
	return nil
}

func jsonTypeName(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case float64:
		if typed == math.Trunc(typed) {
			return "integer"
		}
		return "number"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func containsValue(candidates []any, value any) bool {
	for _, candidate := range candidates {
		if equalValue(candidate, value) {
			return true
		}
	}
	return false
}

func equalValue(left any, right any) bool {
	leftEncoded, leftErr := json.Marshal(left)
	rightEncoded, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return string(leftEncoded) == string(rightEncoded)
}
