package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func generateTypeScriptClient(document openAPIDocument, operations []clientOperation, sourceHash string) []byte {
	var output bytes.Buffer
	output.WriteString(generatedHeader("//", sourceHash))
	fmt.Fprintf(&output, "\n/** OpenAPI info.version represented by this generated transport. */\nexport const CONTRACT_VERSION = %q;\n\n", document.Info.Version)
	for _, schemaName := range sortedMapKeys(document.Components.Schemas) {
		writeTypeScriptSchema(&output, schemaName, document.Components.Schemas[schemaName])
	}
	writeTypeScriptOperationMetadata(&output, operations)
	writeTypeScriptTransport(&output)
	return output.Bytes()
}

func writeTypeScriptSchema(output *bytes.Buffer, name string, value schema) {
	description := value.Description
	if description == "" {
		description = "is the wire representation of the " + splitIdentifierWords(name) + " schema."
	}
	fmt.Fprintf(output, "/** %s */\n", goDocSentence(description))
	if len(value.OneOf) > 0 && value.Type == "" {
		var variants []string
		for _, candidate := range value.OneOf {
			variants = append(variants, typeScriptSchemaType(candidate))
		}
		fmt.Fprintf(output, "export type %s = %s;\n\n", name, strings.Join(variants, " | "))
		return
	}
	if value.Type == "object" && len(value.Properties) > 0 {
		fmt.Fprintf(output, "export interface %s {\n", name)
		required := stringSet(value.Required)
		for _, propertyName := range sortedMapKeys(value.Properties) {
			property := value.Properties[propertyName]
			propertyDescription := property.Description
			if propertyDescription == "" {
				propertyDescription = propertyName + " is the canonical JSON field."
			}
			fmt.Fprintf(output, "  /** %s */\n", goDocSentence(propertyDescription))
			optional := "?"
			if _, exists := required[propertyName]; exists {
				optional = ""
			}
			fmt.Fprintf(output, "  %s%s: %s;\n", propertyName, optional, typeScriptSchemaType(property))
		}
		output.WriteString("}\n\n")
		return
	}
	if len(value.Enum) > 0 {
		values := make([]string, 0, len(value.Enum))
		for _, candidate := range value.Enum {
			values = append(values, typeScriptString(candidate))
		}
		fmt.Fprintf(output, "export type %s = %s;\n\n", name, strings.Join(values, " | "))
		return
	}
	fmt.Fprintf(output, "export type %s = %s;\n\n", name, typeScriptSchemaType(value))
}

func typeScriptSchemaType(value schema) string {
	if value.Ref != "" {
		return strings.TrimPrefix(value.Ref, "#/components/schemas/")
	}
	if len(value.Const) > 0 {
		var constant string
		if json.Unmarshal(value.Const, &constant) == nil {
			return typeScriptString(constant)
		}
	}
	if len(value.OneOf) > 0 {
		var variants []string
		for _, candidate := range value.OneOf {
			variants = append(variants, typeScriptSchemaType(candidate))
		}
		return strings.Join(variants, " | ")
	}
	switch value.Type {
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	case "array":
		if value.Items == nil {
			return "readonly JSONValue[]"
		}
		return "readonly " + typeScriptSchemaType(*value.Items) + "[]"
	case "object":
		if len(value.AdditionalProperties) > 0 && string(value.AdditionalProperties) != "false" {
			var additionalSchema schema
			if json.Unmarshal(value.AdditionalProperties, &additionalSchema) == nil && (additionalSchema.Type != "" || additionalSchema.Ref != "") {
				return "Readonly<Record<string, " + typeScriptSchemaType(additionalSchema) + ">>"
			}
		}
		return "Readonly<Record<string, JSONValue>>"
	default:
		return "JSONValue"
	}
}

func writeTypeScriptOperationMetadata(output *bytes.Buffer, operations []clientOperation) {
	output.WriteString(`/** A JSON value accepted by the dependency-free transport helper. */
export type JSONValue = string | number | boolean | null | readonly JSONValue[] | {readonly [key: string]: JSONValue};

/** One canonical path, query, or header parameter. */
export interface OperationParameter {
  /** Exact parameter wire name. */
  readonly name: string;
  /** Parameter placement in the HTTP request. */
  readonly location: "path" | "query" | "header";
  /** Whether the contract requires the parameter. */
  readonly required: boolean;
  /** Component schema name or primitive wire type. */
  readonly schema: string;
}

/** One accepted request body representation. */
export interface OperationMediaType {
  /** Exact HTTP media type. */
  readonly contentType: string;
  /** Component schema name or primitive wire type. */
  readonly schema: string;
}

/** One declared operation response representation. */
export interface OperationResponse {
  /** OpenAPI response status or default. */
  readonly statusCode: string;
  /** Empty when the response has no body. */
  readonly contentType: string;
  /** Empty when the response has no body. */
  readonly schema: string;
}

/** Canonical transport metadata for one OpenAPI operation. */
export interface OperationMetadata {
  /** Stable OpenAPI operationId. */
  readonly operationId: string;
  /** Uppercase HTTP method. */
  readonly method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  /** Versioned path with named placeholders. */
  readonly pathTemplate: string;
  /** Path, query, and header parameters. */
  readonly parameters: readonly OperationParameter[];
  /** Accepted request body representations. */
  readonly requestBody: readonly OperationMediaType[];
  /** Whether the operation requires a request body. */
  readonly requestBodyRequired: boolean;
  /** Every declared status and representation. */
  readonly responses: readonly OperationResponse[];
}

/** Stable operation metadata keyed by OpenAPI operationId. */
export const OPERATIONS = {
`)
	for _, operation := range operations {
		fmt.Fprintf(output, "  /** The %s OpenAPI operation. */\n", operation.ID)
		fmt.Fprintf(output, "  %s: {\n", operation.ID)
		fmt.Fprintf(output, "    operationId: %q,\n    method: %q,\n    pathTemplate: %q,\n", operation.ID, operation.Method, operation.Path)
		output.WriteString("    parameters: [\n")
		for _, parameter := range operation.Parameters {
			fmt.Fprintf(output, "      {name: %q, location: %q, required: %t, schema: %q},\n", parameter.Name, parameter.Location, parameter.Required, parameter.Schema)
		}
		output.WriteString("    ],\n    requestBody: [\n")
		for _, mediaType := range operation.RequestBody {
			fmt.Fprintf(output, "      {contentType: %q, schema: %q},\n", mediaType.ContentType, mediaType.Schema)
		}
		fmt.Fprintf(output, "    ],\n    requestBodyRequired: %t,\n    responses: [\n", operation.RequestBodyRequired)
		for _, response := range operation.Responses {
			fmt.Fprintf(output, "      {statusCode: %q, contentType: %q, schema: %q},\n", response.StatusCode, response.ContentType, response.Schema)
		}
		output.WriteString("    ],\n  },\n")
	}
	output.WriteString("} as const satisfies Readonly<Record<string, OperationMetadata>>;\n\n")
	output.WriteString("/** OperationID is one stable generated OpenAPI operationId. */\n")
	output.WriteString("export type OperationID = keyof typeof OPERATIONS;\n\n")
}

func writeTypeScriptTransport(output *bytes.Buffer) {
	output.WriteString(`/** Wire values supplied to a generated operation. */
export interface TransportRequestOptions {
  /** Values replacing named path placeholders. */
  readonly pathParameters?: Readonly<Record<string, string>>;
  /** Values encoded into the query string. */
  readonly queryParameters?: Readonly<Record<string, string | number | boolean | readonly (string | number | boolean)[]>>;
  /** Operation headers such as Idempotency-Key and If-Match. */
  readonly headers?: Readonly<Record<string, string>>;
  /** Already encoded JSON, binary, or multipart body. */
  readonly body?: BodyInit;
  /** One media type declared by the selected operation. */
  readonly contentType?: string;
  /** Optional cancellation signal owned by the caller. */
  readonly signal?: AbortSignal;
}

/** A non-successful SecondBox HTTP response. */
export class SecondBoxAPIError extends Error {
  /** Raw response retained for bounded caller-controlled decoding. */
  public readonly response: Response;

  /** Creates a transport error without consuming its response body. */
  public constructor(response: Response) {
    super("SecondBox API request failed: status=" + response.status);
    this.name = "SecondBoxAPIError";
    this.response = response;
  }
}

/** Dependency-free fetch transport for the generated SecondBox contract. */
export class SecondBoxClient {
  private readonly baseUrl: URL;
  private readonly token: string;
  private readonly fetcher: typeof fetch;

  /** Validates the endpoint and requires an explicit fetch implementation. */
  public constructor(rawUrl: string, token: string, fetcher: typeof fetch) {
    const baseUrl = new URL(rawUrl);
    if ((baseUrl.protocol !== "http:" && baseUrl.protocol !== "https:") || baseUrl.search !== "" || baseUrl.hash !== "") {
      throw new Error("SecondBox client URL must be an absolute HTTP endpoint without query or fragment");
    }
    if (token === "") {
      throw new Error("SecondBox client service-account token is required");
    }
    this.baseUrl = baseUrl;
    this.token = token;
    this.fetcher = fetcher;
  }

  /** Sends one generated operation and returns the unconsumed successful response. */
  public async send(operation: OperationMetadata, options: TransportRequestOptions = {}): Promise<Response> {
    let path = operation.pathTemplate;
    for (const parameter of operation.parameters) {
      if (parameter.location !== "path") continue;
      const value = options.pathParameters?.[parameter.name];
      if (parameter.required && (value === undefined || value === "")) {
        throw new Error("SecondBox client missing required path parameter " + parameter.name + " for " + operation.operationId);
      }
      path = path.replaceAll("{" + parameter.name + "}", encodeURIComponent(value ?? ""));
    }
    if (path.includes("{")) {
      throw new Error("SecondBox client has unresolved path template " + path + " for " + operation.operationId);
    }

    const endpoint = new URL(path, this.baseUrl);
    for (const [name, value] of Object.entries(options.queryParameters ?? {})) {
      if (Array.isArray(value)) {
        for (const item of value) endpoint.searchParams.append(name, String(item));
      } else {
        endpoint.searchParams.set(name, String(value));
      }
    }
    let contentType = options.contentType;
    if (options.body !== undefined && contentType === undefined && operation.requestBody.length === 1) {
      contentType = operation.requestBody[0]?.contentType;
    }
    if (contentType !== undefined && !operation.requestBody.some((candidate) => candidate.contentType === contentType)) {
      throw new Error("SecondBox client content type " + contentType + " is not declared for " + operation.operationId);
    }
    const headers = new Headers(options.headers);
    headers.set("Authorization", "Bearer " + this.token);
    if (contentType !== undefined) headers.set("Content-Type", contentType);

    const response = await this.fetcher(endpoint, {
      method: operation.method,
      headers,
      body: options.body,
      signal: options.signal,
    });
    if (!response.ok) throw new SecondBoxAPIError(response);
    return response;
  }
}

/** Encodes a generated request model as an application/json body. */
export function encodeJSONBody(value: unknown): string {
  return JSON.stringify(value);
}
`)
}
