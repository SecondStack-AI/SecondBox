package api

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SecondStack-AI/SecondBox/internal/pagination"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// maximumProblemDetails is the OpenAPI Problem.details bound.
const maximumProblemDetails = 32

// maximumProblemTitleBytes is the OpenAPI Problem.title bound.
const maximumProblemTitleBytes = 256

// maximumProblemDetailFieldBytes is the OpenAPI ProblemDetail.field bound.
const maximumProblemDetailFieldBytes = 256

// maximumEchoedJSONFieldNameBytes bounds a client-supplied member name echoed
// in a Problem detail reason.
const maximumEchoedJSONFieldNameBytes = 128

// invalidRequestProblemDetails reports each InvalidFieldError in err's tree,
// in depth-first order and bounded by maximumProblemDetails. A single named
// input becomes the title, when it fits, so clients that render only the title
// still see it.
func invalidRequestProblemDetails(err error, title string) (string, []contracts.ProblemDetail) {
	var fields []*ports.InvalidFieldError
	var collect func(error)
	collect = func(err error) {
		if len(fields) == maximumProblemDetails {
			return
		}
		switch current := err.(type) {
		case *ports.InvalidFieldError:
			fields = append(fields, current)
		case interface{ Unwrap() []error }:
			for _, wrapped := range current.Unwrap() {
				collect(wrapped)
			}
		case interface{ Unwrap() error }:
			collect(current.Unwrap())
		}
	}
	collect(err)
	if len(fields) == 0 && errors.Is(err, pagination.ErrInvalidListCursor) {
		fields = append(fields, &ports.InvalidFieldError{Field: "cursor", Reason: "is malformed, stale, or belongs to another list"})
	}
	if len(fields) == 0 {
		return title, nil
	}
	if len(fields) == 1 && len(fields[0].Error()) <= maximumProblemTitleBytes {
		title = fields[0].Error()
	}
	details := make([]contracts.ProblemDetail, 0, len(fields))
	for _, field := range fields {
		details = append(details, contracts.ProblemDetail{Field: field.Field, Reason: field.Reason})
	}
	return title, details
}

// jsonRequestBodyFieldError names the request input that strict JSON decoding
// refused without echoing decoder diagnostics, which expose Go types.
// encoding/json reports an unknown member by its name alone, without the path
// to its enclosing object, so the detail names the body and quotes the member.
// Contracts decoder refusals and type mismatches are located from the body
// and destination type: a custom decoder that decodes a member through its own
// decoder hides that member from encoding/json's path, and encoding/json never
// reports array indices.
func jsonRequestBodyFieldError(err error, body []byte, destination reflect.Type) *ports.InvalidFieldError {
	var typeError *json.UnmarshalTypeError
	var syntaxError *json.SyntaxError
	var refused *contracts.RequestFieldError
	var timeError *time.ParseError
	switch {
	case errors.As(err, &refused):
		path := locateRefusedJSONMember(body, destination, func(err error) bool {
			var member *contracts.RequestFieldError
			return errors.As(err, &member) && member.Error() == refused.Error()
		})
		if refused.Field != "" {
			path = append(path, refused.Field)
		}
		return &ports.InvalidFieldError{Field: boundedJSONPath(path), Reason: refused.Reason}
	case errors.As(err, &timeError), strings.HasPrefix(err.Error(), "Time.UnmarshalJSON: "):
		// time.Time reports neither kind of failure with a path; the member
		// that reproduces the identical error is the refused timestamp.
		path := locateRefusedJSONMember(body, destination, func(member error) bool {
			return member != nil && member.Error() == err.Error()
		})
		return &ports.InvalidFieldError{Field: boundedJSONPath(path), Reason: "must be an RFC 3339 timestamp string"}
	case errors.Is(err, io.EOF):
		return &ports.InvalidFieldError{Field: "body", Reason: "is required"}
	case errors.As(err, &syntaxError), errors.Is(err, io.ErrUnexpectedEOF):
		return &ports.InvalidFieldError{Field: "body", Reason: "must be one well-formed JSON object of at most 1 MiB"}
	case errors.As(err, &typeError):
		path := locateRefusedJSONMember(body, destination, func(err error) bool {
			var member *json.UnmarshalTypeError
			return errors.As(err, &member) && member.Value == typeError.Value && member.Type == typeError.Type
		})
		if len(path) == 0 {
			path = []string{typeError.Field}
		}
		field := boundedJSONPath(path)
		return &ports.InvalidFieldError{Field: field, Reason: "must be " + jsonValueKind(typeError.Type)}
	}
	if quoted, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		name, unquoteErr := strconv.Unquote(quoted)
		if unquoteErr == nil && name != "" && len(name) <= maximumEchoedJSONFieldNameBytes && utf8.ValidString(name) {
			return &ports.InvalidFieldError{Field: "body", Reason: "contains unknown field " + strconv.Quote(name)}
		}
		return &ports.InvalidFieldError{Field: "body", Reason: "contains an unknown field"}
	}
	return &ports.InvalidFieldError{Field: "body", Reason: "does not match the request schema"}
}

// locateRefusedJSONMember returns the path, below data decoded as
// destination, of the value whose decoding produced the refusal. It decodes one object member, array element, or map value at
// a time and descends only into one that reproduces the refusal, so a sibling's
// different failure cannot redirect it. Members match struct fields
// case-insensitively, as encoding/json does, and the path uses the document's
// own member names. Array elements are path segments of the form "[index]". It
// runs only after decoding has already failed.
func locateRefusedJSONMember(data []byte, destination reflect.Type, reproduces func(error) bool) []string {
	descend := func(segment string, member []byte, memberType reflect.Type) []string {
		return append([]string{segment}, locateRefusedJSONMember(member, memberType, reproduces)...)
	}
	elementType := destination
	for elementType.Kind() == reflect.Pointer {
		elementType = elementType.Elem()
	}
	switch elementType.Kind() {
	case reflect.Struct:
		var members map[string]json.RawMessage
		if json.Unmarshal(data, &members) != nil {
			return nil
		}
		for index := range elementType.NumField() {
			field := elementType.Field(index)
			name, embedded, ok := jsonMemberName(field)
			if !ok {
				continue
			}
			if embedded {
				if path := locateRefusedJSONMember(data, field.Type, reproduces); path != nil {
					return path
				}
				continue
			}
			key, member, present := jsonObjectMember(members, name)
			if present && reproduces(json.Unmarshal(member, reflect.New(field.Type).Interface())) {
				return descend(key, member, field.Type)
			}
		}
	case reflect.Slice, reflect.Array:
		var elements []json.RawMessage
		if json.Unmarshal(data, &elements) != nil {
			return nil
		}
		for index, element := range elements {
			if reproduces(json.Unmarshal(element, reflect.New(elementType.Elem()).Interface())) {
				return descend("["+strconv.Itoa(index)+"]", element, elementType.Elem())
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if json.Unmarshal(data, &values) != nil {
			return nil
		}
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if len(key) > maximumEchoedJSONFieldNameBytes || !utf8.ValidString(key) {
				continue
			}
			if reproduces(json.Unmarshal(values[key], reflect.New(elementType.Elem()).Interface())) {
				return descend(key, values[key], elementType.Elem())
			}
		}
	}
	return nil
}

// jsonObjectMember finds the member encoding/json would decode into a struct
// field named name: an exact key, otherwise a case-insensitive one.
func jsonObjectMember(members map[string]json.RawMessage, name string) (string, json.RawMessage, bool) {
	if member, ok := members[name]; ok {
		return name, member, true
	}
	for key, member := range members {
		if strings.EqualFold(key, name) {
			return key, member, true
		}
	}
	return "", nil, false
}

// boundedJSONPath joins member names with dots, attaches array indices to the
// member they index, and names the body when the path is empty or exceeds the
// Problem detail field bound.
func boundedJSONPath(path []string) string {
	var joined strings.Builder
	for index, segment := range path {
		if index > 0 && !strings.HasPrefix(segment, "[") {
			joined.WriteByte('.')
		}
		joined.WriteString(segment)
	}
	if joined.Len() == 0 || joined.Len() > maximumProblemDetailFieldBytes {
		return "body"
	}
	return joined.String()
}

// jsonMemberName reports the JSON member a struct field decodes, or that an
// untagged embedded struct promotes its own members.
func jsonMemberName(field reflect.StructField) (string, bool, bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" && field.Anonymous {
		embeddedType := field.Type
		for embeddedType.Kind() == reflect.Pointer {
			embeddedType = embeddedType.Elem()
		}
		if embeddedType.Kind() == reflect.Struct {
			return "", true, true
		}
	}
	if !field.IsExported() {
		return "", false, false
	}
	if name == "" {
		name = field.Name
	}
	return name, false, true
}

// jsonValueKind describes the JSON value a Go destination type accepts.
func jsonValueKind(destination reflect.Type) string {
	if destination == nil {
		return "a valid JSON value"
	}
	for destination.Kind() == reflect.Pointer {
		destination = destination.Elem()
	}
	switch destination.Kind() {
	case reflect.String:
		return "a JSON string"
	case reflect.Bool:
		return "a JSON boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "a JSON integer in range"
	case reflect.Float32, reflect.Float64:
		return "a JSON number in range"
	case reflect.Slice, reflect.Array:
		return "a JSON array"
	case reflect.Map, reflect.Struct:
		return "a JSON object"
	default:
		return "a valid JSON value"
	}
}
