package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func generatePythonClient(document openAPIDocument, operations []clientOperation, sourceHash string) []byte {
	var output bytes.Buffer
	output.WriteString(generatedHeader("#", sourceHash))
	output.WriteString(`
from __future__ import annotations

import json
from dataclasses import dataclass
from typing import BinaryIO, Final, Literal, Mapping, NotRequired, TypeAlias, TypedDict
from urllib import error, parse, request

`)
	fmt.Fprintf(&output, "CONTRACT_VERSION: Final[str] = %q\n\n", document.Info.Version)
	output.WriteString("JSONPrimitive: TypeAlias = str | int | float | bool | None\n")
	output.WriteString("JSONValue: TypeAlias = JSONPrimitive | list[\"JSONValue\"] | dict[str, \"JSONValue\"]\n\n")
	for _, schemaName := range sortedMapKeys(document.Components.Schemas) {
		writePythonSchema(&output, schemaName, document.Components.Schemas[schemaName])
	}
	writePythonOperationMetadata(&output, operations)
	writePythonTransport(&output)
	return output.Bytes()
}

func writePythonSchema(output *bytes.Buffer, name string, value schema) {
	description := value.Description
	if description == "" {
		description = "is the wire representation of the " + splitIdentifierWords(name) + " schema."
	}
	if len(value.OneOf) > 0 && value.Type == "" {
		var variants []string
		for _, candidate := range value.OneOf {
			variants = append(variants, pythonSchemaType(candidate))
		}
		fmt.Fprintf(output, "# %s\n%s: TypeAlias = %q\n\n", goDocSentence(description), name, strings.Join(variants, " | "))
		return
	}
	if value.Type == "object" && len(value.Properties) > 0 {
		fmt.Fprintf(output, "class %s(TypedDict):\n", name)
		fmt.Fprintf(output, "    \"\"\"%s\"\"\"\n\n", escapePythonDocstring(goDocSentence(description)))
		required := stringSet(value.Required)
		for _, propertyName := range sortedMapKeys(value.Properties) {
			propertyType := pythonSchemaType(value.Properties[propertyName])
			if _, exists := required[propertyName]; !exists {
				propertyType = "NotRequired[" + propertyType + "]"
			}
			fmt.Fprintf(output, "    %s: %s\n", propertyName, propertyType)
		}
		output.WriteString("\n\n")
		return
	}
	if len(value.Enum) > 0 {
		values := make([]string, 0, len(value.Enum))
		for _, candidate := range value.Enum {
			values = append(values, pythonString(candidate))
		}
		fmt.Fprintf(output, "# %s\n%s: TypeAlias = Literal[%s]\n\n", goDocSentence(description), name, strings.Join(values, ", "))
		return
	}
	fmt.Fprintf(output, "# %s\n%s: TypeAlias = %s\n\n", goDocSentence(description), name, pythonSchemaType(value))
}

func pythonSchemaType(value schema) string {
	if value.Ref != "" {
		return strings.TrimPrefix(value.Ref, "#/components/schemas/")
	}
	if len(value.Const) > 0 {
		var constant string
		if json.Unmarshal(value.Const, &constant) == nil {
			return "Literal[" + pythonString(constant) + "]"
		}
	}
	if len(value.OneOf) > 0 {
		var variants []string
		for _, candidate := range value.OneOf {
			variants = append(variants, pythonSchemaType(candidate))
		}
		return strings.Join(variants, " | ")
	}
	switch value.Type {
	case "string":
		return "str"
	case "integer":
		return "int"
	case "number":
		return "float"
	case "boolean":
		return "bool"
	case "array":
		if value.Items == nil {
			return "list[JSONValue]"
		}
		return "list[" + pythonSchemaType(*value.Items) + "]"
	case "object":
		if len(value.AdditionalProperties) > 0 && string(value.AdditionalProperties) != "false" {
			var additionalSchema schema
			if json.Unmarshal(value.AdditionalProperties, &additionalSchema) == nil && (additionalSchema.Type != "" || additionalSchema.Ref != "") {
				return "dict[str, " + pythonSchemaType(additionalSchema) + "]"
			}
		}
		return "dict[str, JSONValue]"
	default:
		return "JSONValue"
	}
}

func writePythonOperationMetadata(output *bytes.Buffer, operations []clientOperation) {
	output.WriteString(`@dataclass(frozen=True)
class OperationParameter:
    """Describes one canonical path, query, or header parameter."""

    name: str
    location: Literal["path", "query", "header"]
    required: bool
    schema: str


@dataclass(frozen=True)
class OperationMediaType:
    """Describes one accepted request body representation."""

    content_type: str
    schema: str


@dataclass(frozen=True)
class OperationResponse:
    """Describes one declared operation response representation."""

    status_code: str
    content_type: str
    schema: str


@dataclass(frozen=True)
class OperationMetadata:
    """Provides canonical transport metadata for one OpenAPI operation."""

    operation_id: str
    method: Literal["GET", "POST", "PUT", "PATCH", "DELETE"]
    path_template: str
    parameters: tuple[OperationParameter, ...]
    request_body: tuple[OperationMediaType, ...]
    request_body_required: bool
    responses: tuple[OperationResponse, ...]


OPERATIONS: Final[Mapping[str, OperationMetadata]] = {
`)
	for _, operation := range operations {
		fmt.Fprintf(output, "    %q: OperationMetadata(\n", operation.ID)
		fmt.Fprintf(output, "        operation_id=%q,\n        method=%q,\n        path_template=%q,\n", operation.ID, operation.Method, operation.Path)
		output.WriteString("        parameters=(\n")
		for _, parameter := range operation.Parameters {
			fmt.Fprintf(output, "            OperationParameter(name=%q, location=%q, required=%s, schema=%q),\n", parameter.Name, parameter.Location, pythonBool(parameter.Required), parameter.Schema)
		}
		output.WriteString("        ),\n        request_body=(\n")
		for _, mediaType := range operation.RequestBody {
			fmt.Fprintf(output, "            OperationMediaType(content_type=%q, schema=%q),\n", mediaType.ContentType, mediaType.Schema)
		}
		fmt.Fprintf(output, "        ),\n        request_body_required=%s,\n        responses=(\n", pythonBool(operation.RequestBodyRequired))
		for _, response := range operation.Responses {
			fmt.Fprintf(output, "            OperationResponse(status_code=%q, content_type=%q, schema=%q),\n", response.StatusCode, response.ContentType, response.Schema)
		}
		output.WriteString("        ),\n    ),\n")
	}
	output.WriteString("}\n\n\n")
}

func writePythonTransport(output *bytes.Buffer) {
	output.WriteString(`@dataclass(frozen=True)
class TransportResponse:
    """Contains a successful unconsumed-style SecondBox response as bounded bytes."""

    status_code: int
    headers: Mapping[str, str]
    body: bytes


class SecondBoxAPIError(Exception):
    """Carries a non-successful SecondBox response without approximating its problem type."""

    def __init__(self, status_code: int, headers: Mapping[str, str], body: bytes) -> None:
        super().__init__(f"SecondBox API request failed: status={status_code}")
        self.status_code = status_code
        self.headers = headers
        self.body = body


class SecondBoxClient:
    """Provides a dependency-free urllib transport over generated operation metadata."""

    def __init__(self, raw_url: str, token: str, timeout_seconds: float) -> None:
        endpoint = parse.urlsplit(raw_url)
        if endpoint.scheme not in ("http", "https") or endpoint.netloc == "" or endpoint.query != "" or endpoint.fragment != "":
            raise ValueError("SecondBox client URL must be an absolute HTTP endpoint without query or fragment")
        if token == "":
            raise ValueError("SecondBox client service-account token is required")
        if timeout_seconds <= 0:
            raise ValueError("SecondBox client timeout_seconds must be positive")
        self._base_url = raw_url.rstrip("/")
        self._token = token
        self._timeout_seconds = timeout_seconds

    def send(
        self,
        operation: OperationMetadata,
        *,
        path_parameters: Mapping[str, str] | None = None,
        query_parameters: Mapping[str, str | int | bool | list[str] | list[int]] | None = None,
        headers: Mapping[str, str] | None = None,
        body: bytes | BinaryIO | None = None,
        content_type: str | None = None,
    ) -> TransportResponse:
        """Sends one generated operation and returns its bounded successful response."""
        path = operation.path_template
        supplied_path_parameters = path_parameters or {}
        for parameter in operation.parameters:
            if parameter.location != "path":
                continue
            value = supplied_path_parameters.get(parameter.name)
            if parameter.required and (value is None or value == ""):
                raise ValueError(
                    f"SecondBox client missing required path parameter {parameter.name} for {operation.operation_id}"
                )
            path = path.replace("{" + parameter.name + "}", parse.quote(value or "", safe=""))
        if "{" in path:
            raise ValueError(f"SecondBox client has unresolved path template {path} for {operation.operation_id}")

        query = parse.urlencode(query_parameters or {}, doseq=True)
        endpoint = self._base_url + path
        if query != "":
            endpoint += "?" + query
        selected_content_type = content_type
        if body is not None and selected_content_type is None and len(operation.request_body) == 1:
            selected_content_type = operation.request_body[0].content_type
        if selected_content_type is not None and all(
            candidate.content_type != selected_content_type for candidate in operation.request_body
        ):
            raise ValueError(
                f"SecondBox client content type {selected_content_type} is not declared for {operation.operation_id}"
            )
        request_headers = dict(headers or {})
        request_headers["Authorization"] = "Bearer " + self._token
        if selected_content_type is not None:
            request_headers["Content-Type"] = selected_content_type
        payload = body.read() if hasattr(body, "read") else body
        call = request.Request(endpoint, data=payload, method=operation.method, headers=request_headers)
        try:
            with request.urlopen(call, timeout=self._timeout_seconds) as response:
                response_body = _read_bounded_response(response)
                return TransportResponse(
                    status_code=response.status,
                    headers=dict(response.headers.items()),
                    body=response_body,
                )
        except error.HTTPError as failure:
            failure_body = _read_bounded_response(failure)
            raise SecondBoxAPIError(
                status_code=failure.code,
                headers=dict(failure.headers.items()),
                body=failure_body,
            ) from failure


def encode_json_body(value: JSONValue) -> bytes:
    """Encodes a JSON value for a generated application/json request."""
    return json.dumps(value, separators=(",", ":")).encode("utf-8")


def _read_bounded_response(response: BinaryIO) -> bytes:
    maximum_response_bytes = 64 << 20
    body = response.read(maximum_response_bytes + 1)
    if len(body) > maximum_response_bytes:
        raise ValueError(f"SecondBox client response exceeds {maximum_response_bytes} bytes")
    return body
`)
}

func escapePythonDocstring(value string) string {
	return strings.ReplaceAll(value, `"""`, `\"\"\"`)
}

func pythonBool(value bool) string {
	if value {
		return "True"
	}
	return "False"
}
