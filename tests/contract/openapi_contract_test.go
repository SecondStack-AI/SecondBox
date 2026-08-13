package contract_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type openAPIDocument map[string]any

func loadOpenAPIContract(t *testing.T) openAPIDocument {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate SecondBox OpenAPI contract test source")
	}
	contractPath := filepath.Join(filepath.Dir(sourceFile), "..", "..", "contracts", "openapi", "v1", "secondbox.openapi.json")
	contents, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read canonical SecondBox OpenAPI contract: %v", err)
	}
	var document openAPIDocument
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse canonical SecondBox OpenAPI contract: %v", err)
	}
	return document
}

func object(t *testing.T, value any, context string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s must be an object, got %T", context, value)
	}
	return result
}

func array(t *testing.T, value any, context string) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok {
		t.Fatalf("%s must be an array, got %T", context, value)
	}
	return result
}

func resolveLocalReference(t *testing.T, document openAPIDocument, reference string) any {
	t.Helper()
	if !strings.HasPrefix(reference, "#/") {
		t.Fatalf("public OpenAPI contains non-local reference %q", reference)
	}
	var resolved any = map[string]any(document)
	for _, encodedSegment := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encodedSegment, "~1", "/"), "~0", "~")
		container := object(t, resolved, "OpenAPI reference container")
		next, exists := container[segment]
		if !exists {
			t.Fatalf("public OpenAPI contains unresolved reference %q", reference)
		}
		resolved = next
	}
	return resolved
}

func componentSchema(t *testing.T, document openAPIDocument, name string) map[string]any {
	t.Helper()
	components := object(t, document["components"], "components")
	schemas := object(t, components["schemas"], "components.schemas")
	return object(t, schemas[name], "components.schemas."+name)
}

func walkJSON(value any, visit func(map[string]any)) {
	switch node := value.(type) {
	case map[string]any:
		visit(node)
		for _, child := range node {
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range node {
			walkJSON(child, visit)
		}
	}
}

func reachableSchemaNames(t *testing.T, document openAPIDocument, root any) map[string]bool {
	t.Helper()
	reachable := map[string]bool{}
	var discover func(any)
	discover = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			if reference, ok := node["$ref"].(string); ok && strings.HasPrefix(reference, "#/components/schemas/") {
				name := strings.TrimPrefix(reference, "#/components/schemas/")
				if !reachable[name] {
					reachable[name] = true
					discover(resolveLocalReference(t, document, reference))
				}
			}
			for key, child := range node {
				if key != "$ref" {
					discover(child)
				}
			}
		case []any:
			for _, child := range node {
				discover(child)
			}
		}
	}
	discover(root)
	return reachable
}

func validateClosedCreateShape(t *testing.T, schema map[string]any, payload map[string]any) error {
	t.Helper()
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatal("CreateSandboxRequest must remain a closed object")
	}
	properties := object(t, schema["properties"], "CreateSandboxRequest.properties")
	for name := range payload {
		if _, allowed := properties[name]; !allowed {
			return fmt.Errorf("CreateSandboxRequest rejected unknown property %q", name)
		}
	}
	for _, requiredValue := range array(t, schema["required"], "CreateSandboxRequest.required") {
		required := requiredValue.(string)
		if _, present := payload[required]; !present {
			return fmt.Errorf("CreateSandboxRequest rejected missing property %q", required)
		}
	}
	return nil
}

func TestCanonicalOpenAPIProtocolShape(t *testing.T) {
	document := loadOpenAPIContract(t)
	if document["openapi"] != "3.1.0" {
		t.Fatalf("canonical contract must use OpenAPI 3.1.0, got %v", document["openapi"])
	}
	if document["jsonSchemaDialect"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("canonical contract must declare JSON Schema 2020-12, got %v", document["jsonSchemaDialect"])
	}

	t.Run("all local references resolve", func(t *testing.T) {
		referenceCount := 0
		walkJSON(map[string]any(document), func(node map[string]any) {
			if reference, ok := node["$ref"].(string); ok {
				referenceCount++
				resolveLocalReference(t, document, reference)
			}
		})
		if referenceCount < 100 {
			t.Fatalf("canonical contract is unexpectedly small: only %d references", referenceCount)
		}
	})

	t.Run("required public operations have unique identifiers", func(t *testing.T) {
		paths := object(t, document["paths"], "paths")
		operationIDs := map[string]bool{}
		for path, unresolvedPathItem := range paths {
			if !strings.HasPrefix(path, "/v1/") {
				t.Errorf("unversioned public path %q", path)
			}
			pathItem := object(t, unresolvedPathItem, "path item")
			if reference, ok := pathItem["$ref"].(string); ok {
				pathItem = object(t, resolveLocalReference(t, document, reference), "referenced path item")
			}
			for _, method := range []string{"delete", "get", "patch", "post", "put"} {
				operationValue, exists := pathItem[method]
				if !exists {
					continue
				}
				operation := object(t, operationValue, path+" "+method)
				operationID, ok := operation["operationId"].(string)
				if !ok || operationID == "" {
					t.Errorf("%s %s has no operationId", method, path)
				} else if operationIDs[operationID] {
					t.Errorf("duplicate operationId %q", operationID)
				}
				operationIDs[operationID] = true
			}
		}
		for _, required := range []string{
			"listTenants", "createTenant", "getTenant", "suspendTenant", "reactivateTenant",
			"listTenantControllerAuthorities", "createTenantControllerAuthority",
			"getTenantControllerAuthority", "rotateTenantControllerAuthority", "revokeTenantControllerAuthority",
			"listSubjects", "createSubject", "getSubject", "updateSubjectQuota", "closeSubject", "cleanupSubject",
			"listApplicationAuthorities", "createApplicationAuthority", "getApplicationAuthority",
			"rotateApplicationAuthority", "revokeApplicationAuthority", "getTenantUsage", "getDeploymentUsage",
			"createProfile", "reviseProfile", "createSandbox", "updateSandboxMetadata", "startSandbox",
			"drainSandbox", "stopSandbox", "restoreSandboxSnapshot", "getOperation",
			"executeSandboxCommand", "createSandboxExecStream", "readSandboxFile",
			"writeSandboxFile",
			"createSandboxPortSession",
			"getSandboxTiming", "getOperationTiming", "getDeploymentTiming",
		} {
			if !operationIDs[required] {
				t.Errorf("canonical contract is missing operationId %q", required)
			}
		}
	})

	t.Run("create correlation idempotency paging and etag are explicit", func(t *testing.T) {
		components := object(t, document["components"], "components")
		parameters := object(t, components["parameters"], "components.parameters")
		headers := object(t, components["headers"], "components.headers")
		for _, name := range []string{
			"IdempotencyKey", "IfMatch", "PageCursor", "PageLimit", "RequestID",
			"TenantRef", "SubjectRef",
		} {
			if _, exists := parameters[name]; !exists {
				t.Errorf("missing reusable parameter %q", name)
			}
		}
		for _, name := range []string{"ETag", "RequestID"} {
			if _, exists := headers[name]; !exists {
				t.Errorf("missing reusable response header %q", name)
			}
		}
		create := object(t, object(t, document["paths"], "paths")["/v1/sandboxes"], "sandboxes path")
		create = object(t, create["post"], "createSandbox")
		encodedParameters, _ := json.Marshal(create["parameters"])
		if !strings.Contains(string(encodedParameters), "#/components/parameters/IdempotencyKey") {
			t.Error("createSandbox must require the reusable Idempotency-Key header")
		}
	})

	t.Run("administrative mutation replays are observable", func(t *testing.T) {
		required := map[string]bool{
			"createProfile": true, "reviseProfile": true, "disableProfile": true,
			"createTenant": true, "suspendTenant": true, "reactivateTenant": true,
			"createTenantControllerAuthority": true, "rotateTenantControllerAuthority": true,
			"revokeTenantControllerAuthority": true, "createSubject": true, "updateSubjectQuota": true,
			"closeSubject": true, "cleanupSubject": true,
			"createApplicationAuthority": true, "rotateApplicationAuthority": true,
			"revokeApplicationAuthority": true,
		}
		paths := object(t, document["paths"], "paths")
		for path, pathValue := range paths {
			pathItem := object(t, pathValue, path)
			if reference, ok := pathItem["$ref"].(string); ok {
				pathItem = object(t, resolveLocalReference(t, document, reference), path)
			}
			for _, method := range []string{"patch", "post", "put"} {
				operationValue, exists := pathItem[method]
				if !exists {
					continue
				}
				operation := object(t, operationValue, method+" "+path)
				operationID, _ := operation["operationId"].(string)
				if !required[operationID] {
					continue
				}
				parameters, _ := json.Marshal(operation["parameters"])
				if !strings.Contains(string(parameters), "#/components/parameters/IdempotencyKey") {
					t.Errorf("%s must require Idempotency-Key", operationID)
				}
				responses := object(t, operation["responses"], operationID+".responses")
				for status, responseValue := range responses {
					if !strings.HasPrefix(status, "2") {
						continue
					}
					response := resolveOpenAPIObject(t, document, responseValue, operationID+" "+status)
					headers := object(t, response["headers"], operationID+" "+status+" headers")
					if _, exists := headers["Idempotency-Replayed"]; !exists {
						t.Errorf("%s %s must expose Idempotency-Replayed", operationID, status)
					}
				}
				delete(required, operationID)
			}
		}
		for operationID := range required {
			t.Errorf("missing administrative mutation %s", operationID)
		}
	})

	t.Run("application routes require ownership headers and removed identity vocabulary stays absent", func(t *testing.T) {
		paths := object(t, document["paths"], "paths")
		for path, pathValue := range paths {
			if strings.HasPrefix(path, "/v1/tenants") ||
				strings.HasPrefix(path, "/v1/subjects") ||
				strings.HasPrefix(path, "/v1/application-authorities") ||
				path == "/v1/usage" || path == "/v1/deployment-usage" {
				continue
			}
			pathItem := object(t, pathValue, path)
			if reference, ok := pathItem["$ref"].(string); ok {
				pathItem = object(t, resolveLocalReference(t, document, reference), path)
			}
			encoded, _ := json.Marshal(pathItem["parameters"])
			for _, reference := range []string{
				"#/components/parameters/TenantRef",
				"#/components/parameters/SubjectRef",
			} {
				if !strings.Contains(string(encoded), reference) {
					t.Errorf("%s does not require %s", path, reference)
				}
			}
		}
		components := object(t, document["components"], "components")
		schemas := object(t, components["schemas"], "components.schemas")
		for _, name := range []string{
			"Project", "ProjectPage", "CreateProjectRequest", "UpdateProjectRequest",
			"ServiceAccount", "ServiceAccountPage", "CreateServiceAccountRequest",
			"UpdateServiceAccountRequest", "APIKey", "APIKeyPage", "CreateAPIKeyRequest",
			"CreateAPIKeyResponse", "ServiceAccountScope",
		} {
			if _, exists := schemas[name]; exists {
				t.Errorf("removed identity schema %s remains public", name)
			}
		}
	})

	t.Run("problem responses are typed and correlated", func(t *testing.T) {
		problem := componentSchema(t, document, "Problem")
		if problem["additionalProperties"] != false {
			t.Error("Problem must reject unstable extension fields")
		}
		required := map[string]bool{}
		for _, value := range array(t, problem["required"], "Problem.required") {
			required[value.(string)] = true
		}
		for _, field := range []string{"type", "title", "status", "code", "requestId", "retryable"} {
			if !required[field] {
				t.Errorf("Problem must require %q", field)
			}
		}
	})

	t.Run("exec outcomes discriminate every terminal condition", func(t *testing.T) {
		outcome := componentSchema(t, document, "ExecOutcome")
		discriminator := object(t, outcome["discriminator"], "ExecOutcome.discriminator")
		if discriminator["propertyName"] != "kind" {
			t.Errorf("ExecOutcome discriminator must be kind, got %v", discriminator["propertyName"])
		}
		mapping := object(t, discriminator["mapping"], "ExecOutcome.discriminator.mapping")
		for _, kind := range []string{"exited", "spawn_failed", "deadline_exceeded", "cancelled", "output_exhausted", "infrastructure_failed"} {
			if _, exists := mapping[kind]; !exists {
				t.Errorf("ExecOutcome is missing terminal kind %q", kind)
			}
		}
		for _, name := range []string{"ExecSpawnFailed", "ExecDeadlineExceeded", "ExecCancelled", "ExecOutputExhausted", "ExecInfrastructureFailed"} {
			properties := object(t, componentSchema(t, document, name)["properties"], name+".properties")
			if _, exists := properties["exitCode"]; exists {
				t.Errorf("%s must not encode failure as a synthetic exit code", name)
			}
		}
		exited := componentSchema(t, document, "ExecExited")
		exitedRequired := map[string]bool{}
		for _, value := range array(t, exited["required"], "ExecExited.required") {
			exitedRequired[value.(string)] = true
		}
		if !exitedRequired["elapsedMilliseconds"] {
			t.Error("ExecExited must expose successful command elapsed time")
		}
	})

	t.Run("public profile omits virtualization backend selection", func(t *testing.T) {
		spec := componentSchema(t, document, "ProfileRevisionSpec")
		properties := object(t, spec["properties"], "ProfileRevisionSpec.properties")
		if _, exists := properties["backend"]; exists {
			t.Fatal("ProfileRevisionSpec exposes backend selection")
		}
	})

	t.Run("startup policy is required and provider neutral", func(t *testing.T) {
		spec := componentSchema(t, document, "ProfileRevisionSpec")
		properties := object(t, spec["properties"], "ProfileRevisionSpec.properties")
		if _, exists := properties["startup"]; !exists {
			t.Fatal("ProfileRevisionSpec has no startup policy")
		}
		required := map[string]bool{}
		for _, value := range array(t, spec["required"], "ProfileRevisionSpec.required") {
			required[value.(string)] = true
		}
		// The control plane has no application default for the startup mode, so
		// the contract itself must refuse a revision that omits it.
		if !required["startup"] {
			t.Error("ProfileRevisionSpec startup policy must be required, not defaulted")
		}
		policy := componentSchema(t, document, "StartupPolicy")
		policyRequired := map[string]bool{}
		for _, value := range array(t, policy["required"], "StartupPolicy.required") {
			policyRequired[value.(string)] = true
		}
		if !policyRequired["mode"] {
			t.Error("StartupPolicy mode must be required")
		}
		modes := map[string]bool{}
		for _, value := range array(
			t, componentSchema(t, document, "StartupMode")["enum"], "StartupMode.enum",
		) {
			modes[value.(string)] = true
		}
		if len(modes) != 2 || !modes["cold_boot"] || !modes["snapshot_resume"] {
			t.Errorf("StartupMode enum = %v, want exactly cold_boot and snapshot_resume", modes)
		}
		// The plan's non-goals forbid an ephemeral Profile or lifecycle concept.
		if modes["ephemeral"] {
			t.Error("StartupMode introduced an ephemeral Profile class")
		}
		encoded, err := json.Marshal([]any{policy, componentSchema(t, document, "StartupMode")})
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(encoded))
		for _, forbidden := range []string{
			"firecracker", "kvm", "jailer", "memoryfile", "memory_file", "vmstate", "vm_state",
			"vsock", "tap", "hostpath", "host_path", "templateid", "template_id", "cputemplate",
			"cachekey", "cache_key", "ephemeral",
		} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("public startup policy exposes %q: %s", forbidden, encoded)
			}
		}
	})

	t.Run("snapshot-resume capacity is a provider-neutral Runner capability", func(t *testing.T) {
		capabilities := map[string]bool{}
		for _, value := range array(
			t,
			object(
				t,
				componentSchema(t, document, "RunnerCapabilityList")["items"],
				"RunnerCapabilityList.items",
			)["enum"],
			"RunnerCapabilityList.items.enum",
		) {
			capabilities[value.(string)] = true
		}
		if !capabilities["snapshot-resume"] {
			t.Error("RunnerPool capabilities cannot declare snapshot-resume capacity")
		}
		runnerCapabilityItems := object(
			t,
			object(
				t,
				object(
					t,
					componentSchema(t, document, "Runner")["properties"],
					"Runner.properties",
				)["capabilities"],
				"Runner.properties.capabilities",
			)["items"],
			"Runner.properties.capabilities.items",
		)
		runnerCapabilities := map[string]bool{}
		for _, value := range array(
			t, runnerCapabilityItems["enum"], "Runner.properties.capabilities.items.enum",
		) {
			runnerCapabilities[value.(string)] = true
		}
		if !runnerCapabilities["snapshot-resume"] {
			t.Error("Runner capabilities cannot advertise snapshot-resume capacity")
		}
	})

	t.Run("timing projections are bounded and provider neutral", func(t *testing.T) {
		components := object(t, document["components"], "components")
		parameters := object(t, components["parameters"], "components.parameters")
		for _, name := range []string{"TimingLimit", "TimingWindowSeconds"} {
			parameter := object(t, parameters[name], "components.parameters."+name)
			if parameter["required"] != true {
				t.Errorf("%s must be required", name)
			}
		}
		var timingSchemas []any
		for _, name := range []string{
			"BootStageTiming", "BootTiming", "OperationStageTiming", "OperationTiming", "ExecTiming",
			"SandboxTiming", "DeploymentTimingSummary",
		} {
			timingSchemas = append(timingSchemas, componentSchema(t, document, name))
		}
		encoded, err := json.Marshal(timingSchemas)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			"firecracker", "kvm", "runnerId", "hostPath", "storageKey",
			"fencingToken", "backendReference",
		} {
			if strings.Contains(string(encoded), forbidden) {
				t.Errorf("public timing schemas contain internal vocabulary %q", forbidden)
			}
		}
	})
}

func TestManagementContractRejectsUnsafeShapes(t *testing.T) {
	document := loadOpenAPIContract(t)

	t.Run("management request objects reject unknown fields", func(t *testing.T) {
		for _, name := range []string{
			"CreateTenantRequest", "CreateTenantControllerAuthorityRequest",
			"CreateSubjectRequest", "CreateApplicationAuthorityRequest",
		} {
			schema := componentSchema(t, document, name)
			if schema["type"] != "object" || schema["additionalProperties"] != false {
				t.Errorf("%s must be a closed object", name)
			}
		}
	})

	t.Run("controller routes accept no tenant assertion", func(t *testing.T) {
		for path, pathValue := range object(t, document["paths"], "paths") {
			if !strings.HasPrefix(path, "/v1/subjects") &&
				!strings.HasPrefix(path, "/v1/application-authorities") &&
				path != "/v1/usage" {
				continue
			}
			pathItem := resolveOpenAPIObject(t, document, pathValue, path)
			encoded, err := json.Marshal(pathItem)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "#/components/parameters/TenantRef") ||
				strings.Contains(string(encoded), "X-SecondBox-Tenant-Ref") {
				t.Errorf("controller route %s accepts a tenant assertion", path)
			}
		}
		applicationCreate := componentSchema(t, document, "CreateApplicationAuthorityRequest")
		if _, exists := object(t, applicationCreate["properties"], "application create properties")["tenantRef"]; exists {
			t.Error("application authority creation accepts a caller-selected tenant")
		}
	})

	t.Run("non-secret authority projections cannot recover credentials", func(t *testing.T) {
		for _, name := range []string{
			"TenantControllerAuthority", "TenantControllerAuthorityPage",
			"ApplicationAuthority", "ApplicationAuthorityPage",
		} {
			encoded, err := json.Marshal(componentSchema(t, document, name))
			if err != nil {
				t.Fatal(err)
			}
			lower := strings.ToLower(string(encoded))
			for _, forbidden := range []string{"bearertoken", "verifier", "secret"} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("non-secret schema %s exposes recoverable credential field %q", name, forbidden)
				}
			}
		}
		for _, name := range []string{"TenantControllerCredentialResponse", "ApplicationCredentialResponse"} {
			properties := object(t, componentSchema(t, document, name)["properties"], name+".properties")
			if len(properties) != 2 || properties["authority"] == nil || properties["bearerToken"] == nil {
				t.Errorf("%s must expose exactly the authority and one-time bearer token", name)
			}
		}
	})

	t.Run("authority creation cannot escalate kind or grants", func(t *testing.T) {
		for _, name := range []string{"CreateTenantControllerAuthorityRequest", "CreateApplicationAuthorityRequest"} {
			properties := object(t, componentSchema(t, document, name)["properties"], name+".properties")
			for _, forbidden := range []string{"kind", "authorityKind", "tenantRef", "grant"} {
				if _, exists := properties[forbidden]; exists {
					t.Errorf("%s permits authority escalation through %q", name, forbidden)
				}
			}
		}
		scopes := map[string]bool{}
		for _, value := range array(t, componentSchema(t, document, "ApplicationScope")["enum"], "ApplicationScope.enum") {
			scopes[value.(string)] = true
		}
		for scope := range scopes {
			if strings.Contains(scope, "tenant") || strings.Contains(scope, "platform") || strings.Contains(scope, "runner") || strings.Contains(scope, "profile") {
				t.Errorf("application scope %q escalates into management authority", scope)
			}
		}
	})

	t.Run("management errors and audit attribution are explicit", func(t *testing.T) {
		codes := map[string]bool{}
		for _, value := range array(t, componentSchema(t, document, "ProblemCode")["enum"], "ProblemCode.enum") {
			codes[value.(string)] = true
		}
		for _, code := range []string{
			"authority_kind_mismatch", "invalid_lifecycle_transition", "resource_expired", "tenant_suspended",
			"grant_escalation_denied", "quota_exceeded", "precondition_failed",
			"cleanup_state_conflict", "management_unavailable", "credential_response_unavailable",
		} {
			if !codes[code] {
				t.Errorf("ProblemCode is missing management error %q", code)
			}
		}
		audit := componentSchema(t, document, "AuditAttribution")
		required := map[string]bool{}
		for _, value := range array(t, audit["required"], "AuditAttribution.required") {
			required[value.(string)] = true
		}
		for _, field := range []string{"actorAuthorityKind", "actorAuthorityId", "tenantRef", "operation", "result", "correlationMetadata"} {
			if !required[field] {
				t.Errorf("AuditAttribution must require %q", field)
			}
		}
		properties := object(t, audit["properties"], "AuditAttribution.properties")
		for _, forbidden := range []string{"bearerToken", "verifier", "secret"} {
			if _, exists := properties[forbidden]; exists {
				t.Errorf("AuditAttribution exposes credential material %q", forbidden)
			}
		}
	})

	t.Run("credential uncertainty procedure is documented", func(t *testing.T) {
		responses := object(t, object(t, document["components"], "components")["responses"], "components.responses")
		for _, name := range []string{"TenantControllerCredentialCreated", "ApplicationCredentialCreated"} {
			description, _ := object(t, responses[name], name)["description"].(string)
			for _, required := range []string{"uncertain", "revoke", "replacement", "cannot be recovered"} {
				if !strings.Contains(strings.ToLower(description), required) {
					t.Errorf("%s does not document %q", name, required)
				}
			}
		}
	})
}

func TestSandboxCreateRejectsInfrastructureAuthorityOverrides(t *testing.T) {
	document := loadOpenAPIContract(t)
	createSchema := componentSchema(t, document, "CreateSandboxRequest")
	properties := object(t, createSchema["properties"], "CreateSandboxRequest.properties")
	if len(properties) != 3 ||
		properties["profile"] == nil ||
		properties["metadata"] == nil ||
		properties["sourceSnapshotId"] == nil {
		t.Fatalf(
			"CreateSandboxRequest properties must be profile, metadata, and sourceSnapshotId, got %v",
			properties,
		)
	}
	if err := validateClosedCreateShape(t, createSchema, map[string]any{"profile": "standard", "metadata": map[string]string{}}); err != nil {
		t.Fatalf("valid profile-based create was rejected: %v", err)
	}
	if err := validateClosedCreateShape(t, createSchema, map[string]any{
		"profile": "standard", "metadata": map[string]string{},
		"sourceSnapshotId": "snapshot-one",
	}); err != nil {
		t.Fatalf("valid Snapshot-seeded create was rejected: %v", err)
	}

	for _, forbidden := range []string{
		"backend", "backendRef", "vcpuCount", "environmentId", "fencingToken",
		"hostPath", "image", "imageRef", "idempotencyKey", "lifecycle",
		"lifecyclePolicyId", "memoryBytes", "network", "placement",
		"resourceClassId", "resources", "runnerCredential", "runnerId",
		"runnerPool", "secondStackProjectId", "storageRef", "subjectRef", "tenantRef",
	} {
		t.Run(forbidden, func(t *testing.T) {
			payload := map[string]any{"profile": "standard", "metadata": map[string]string{}, forbidden: "caller-controlled"}
			err := validateClosedCreateShape(t, createSchema, payload)
			if err == nil || !strings.Contains(err.Error(), forbidden) {
				t.Fatalf("CreateSandboxRequest did not reject %q: %v", forbidden, err)
			}
		})
	}
}

func TestCanonicalMutationResponsesMatchDurableResources(t *testing.T) {
	document := loadOpenAPIContract(t)
	paths := object(t, document["paths"], "paths")
	tests := []struct {
		name       string
		path       string
		method     string
		statusCode string
		schema     string
	}{
		{
			name: "Sandbox deletion returns its durable lifecycle Operation",
			path: "/v1/sandboxes/{sandboxId}", method: "delete",
			statusCode: "202", schema: "Operation",
		},
		{
			name: "streaming Exec cancellation returns its durable session",
			path: "/v1/sandboxes/{sandboxId}/exec-streams/{execSessionId}:cancel", method: "post",
			statusCode: "202", schema: "ExecStreamSession",
		},
		{
			name: "Terminal cancellation returns its durable session",
			path: "/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}", method: "delete",
			statusCode: "202", schema: "TerminalSession",
		},
		{
			name: "Profile revision updates the existing Profile",
			path: "/v1/profiles/{profileName}:revise", method: "post",
			statusCode: "200", schema: "Profile",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pathItem := object(t, paths[test.path], test.path)
			operation := object(t, pathItem[test.method], test.method+" "+test.path)
			responses := object(t, operation["responses"], "responses")
			response := object(t, responses[test.statusCode], "response "+test.statusCode)
			if reference, ok := response["$ref"].(string); ok {
				response = object(t, resolveLocalReference(t, document, reference), "resolved response")
			}
			content := object(t, response["content"], "response content")
			mediaType := object(t, content["application/json"], "application/json")
			schema := object(t, mediaType["schema"], "response schema")
			wantReference := "#/components/schemas/" + test.schema
			if schema["$ref"] != wantReference {
				t.Fatalf("response schema = %v, want %q", schema["$ref"], wantReference)
			}
		})
	}
}

func TestDataPlaneSchemasHideProviderRunnerAndUpstreamAuthority(t *testing.T) {
	document := loadOpenAPIContract(t)
	paths := object(t, document["paths"], "paths")
	reachable := map[string]bool{}
	for path, pathItem := range paths {
		if strings.HasPrefix(path, "/v1/sandboxes") ||
			strings.HasPrefix(path, "/v1/operations") ||
			strings.HasPrefix(path, "/v1/leases") {
			for name := range reachableSchemaNames(t, document, pathItem) {
				reachable[name] = true
			}
		}
	}

	forbiddenProperties := map[string]bool{}
	for _, name := range []string{
		"backend", "backendRef", "environmentId", "fencingToken", "hostPath",
		"image", "imageRef", "lifecyclePolicyId", "placement", "resourceClassId",
		"runnerCredential", "runnerEndpoint", "runnerHost", "runnerId", "runnerToken",
		"secondStackProjectId", "secondStackSubjectId", "storageRef", "subjectRef", "tenantRef",
	} {
		forbiddenProperties[name] = true
	}

	var exposed []string
	for name := range reachable {
		schema := componentSchema(t, document, name)
		encoded, err := json.Marshal(schema)
		if err != nil {
			t.Fatalf("encode reachable schema %s: %v", name, err)
		}
		lowerSchema := strings.ToLower(string(encoded))
		if strings.Contains(lowerSchema, "firecracker") || strings.Contains(lowerSchema, `"kvm"`) {
			exposed = append(exposed, name+":provider-literal")
		}
		walkJSON(schema, func(node map[string]any) {
			propertiesValue, exists := node["properties"]
			if !exists {
				return
			}
			for property := range object(t, propertiesValue, name+".properties") {
				if forbiddenProperties[property] {
					exposed = append(exposed, name+"."+property)
				}
			}
		})
	}
	sort.Strings(exposed)
	if len(exposed) != 0 {
		t.Fatalf("data-plane request/response schemas expose infrastructure authority: %v", exposed)
	}
}

func TestPublicResourcesAndGeneratedSDKsContainNoPrivateWorkspaceAuthority(
	t *testing.T,
) {
	document := loadOpenAPIContract(t)
	for _, name := range []string{
		"Sandbox",
		"Workspace",
		"Snapshot",
		"Operation",
		"Profile",
		"ProfileRevision",
		"ProfileRevisionSpec",
		"Runner",
		"RunnerPool",
	} {
		encoded, err := json.Marshal(componentSchema(t, document, name))
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(encoded))
		for _, forbidden := range []string{
			"homerunner",
			"home_runner",
			"hostpath",
			"host_path",
			"storageref",
			"storage_ref",
			"storagekey",
			"storage_key",
			"workspaceimagesha",
			"workspace_image_sha",
			`"backend"`,
			"firecracker",
			`"kvm"`,
			"reflink",
			`"ext4"`,
			"smolvm",
			"dm-thin",
		} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("public %s schema exposes %q: %s", name, forbidden, encoded)
			}
		}
	}
	runnerProperties := object(
		t,
		componentSchema(t, document, "Runner")["properties"],
		"Runner.properties",
	)
	if _, exists := runnerProperties["id"]; !exists {
		t.Fatal("administrative Runner schema lost its logical Runner ID")
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate SecondBox contract test source")
	}
	repositoryRoot := filepath.Clean(
		filepath.Join(filepath.Dir(sourceFile), "..", ".."),
	)
	for _, relativePath := range []string{
		"pkg/contracts/contracts.go",
		"sdk/go/secondboxclient/wire_types.go",
		"sdk/typescript/transport.ts",
		"sdk/typescript/transport-runtime.ts",
	} {
		contents, err := os.ReadFile(filepath.Join(repositoryRoot, relativePath))
		if err != nil {
			t.Fatalf("read generated representation %s: %v", relativePath, err)
		}
		lower := strings.ToLower(string(contents))
		for _, forbidden := range []string{
			"homerunner",
			"home_runner",
			"hostpath",
			"host_path",
			"storageref",
			"storage_ref",
			"storagekey",
			"storage_key",
			"workspaceimagesha",
			"workspace_image_sha",
			"firecracker",
			"reflink",
			"smolvm",
			"dm-thin",
		} {
			if strings.Contains(lower, forbidden) {
				t.Errorf(
					"generated representation %s exposes %q",
					relativePath,
					forbidden,
				)
			}
		}
	}
}
