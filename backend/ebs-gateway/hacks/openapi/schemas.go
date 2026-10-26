//go:build ignore

package main

import (
	"encoding/json"
	"reflect"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kube-openapi/pkg/validation/spec"

	generated "ebs-apiserver/pkg/generated/openapi"
)

func schemaName(name string) string { return strings.ReplaceAll(name, "/", ".") }

func ref(name string) object { return object{"$ref": "#/components/schemas/" + schemaName(name)} }

func resourceRef(kind string) object { return ref("ebs-api/ebs/v1." + kind) }

func textSchema() object { return object{"type": "string"} }

func record(properties object, required ...string) object {
	schema := object{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func schemas() object {
	result := object{}
	definitions := generated.GetOpenAPIDefinitions(func(name string) spec.Ref {
		return spec.MustCreateRef("#/components/schemas/" + schemaName(name))
	})
	for name, definition := range definitions {
		data, err := json.Marshal(definition.Schema)
		if err != nil {
			fail(err)
		}
		var schema object
		if err := json.Unmarshal(data, &schema); err != nil {
			fail(err)
		}
		result[schemaName(name)] = schema
	}
	for _, value := range []any{metav1.ObjectMeta{}, metav1.ListMeta{}, metav1.Condition{}, metav1.DeleteOptions{}, metav1.Status{}} {
		metadataSchema(reflect.TypeOf(value), result)
	}
	meta := result[schemaName("k8s.io/apimachinery/pkg/apis/meta/v1.ObjectMeta")].(map[string]any)["properties"].(map[string]any)
	for _, field := range []string{"uid", "generation", "creationTimestamp", "deletionTimestamp", "managedFields"} {
		meta[field].(map[string]any)["description"] = "Server-managed. Echo unchanged on PUT; do not set on creation."
	}
	requestSchemas(result)
	result[schemaName("k8s.io/apimachinery/pkg/apis/meta/v1.Time")] = object{"type": "string", "format": "date-time"}
	result[schemaName("k8s.io/apimachinery/pkg/runtime.RawExtension")] = object{"type": "object", "additionalProperties": true}
	result["GatewayError"] = record(object{"error": textSchema()}, "error")
	result["RegisteredUser"] = record(object{"username": textSchema()}, "username")
	result["RegisteredMachine"] = record(object{"name": textSchema()}, "name")
	result["WatchEvent"] = record(object{
		"type":   object{"type": "string", "enum": []string{"ADDED", "MODIFIED", "DELETED", "BOOKMARK", "ERROR"}},
		"object": object{"anyOf": []object{resourceRef("Job"), ref("k8s.io/apimachinery/pkg/apis/meta/v1.Status")}},
	}, "type", "object")
	result["JobAbort"] = record(object{
		"uid":    object{"type": "string", "minLength": 1, "description": "UID from the latest Job, required as a precondition."},
		"reason": object{"type": "string", "maxLength": 1024},
	}, "uid")
	endpoint := record(object{"runnerInternalURL": textSchema(), "runnerExternalURL": textSchema()},
		"runnerInternalURL", "runnerExternalURL")
	result["ServiceEndpoints"] = record(object{
		"artifactManagers": object{"type": "object", "additionalProperties": endpoint}, "gitServer": endpoint,
	}, "artifactManagers", "gitServer")
	credentials := object{"username": textSchema(), "password": object{"type": "string", "format": "password"}}
	result["LoginRequest"] = record(credentials, "username", "password")
	result["RegisterRequest"] = record(object{
		"username":    object{"type": "string", "maxLength": 63, "pattern": "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"},
		"password":    object{"type": "string", "format": "password", "description": "12–128 UTF-8 bytes."},
		"displayName": textSchema(), "email": object{"type": "string", "format": "email"},
	}, "username", "password")
	result["LoginResponse"] = record(object{
		"token": textSchema(), "tokenType": object{"type": "string", "enum": []string{"Bearer"}},
		"expiresIn": object{"type": "integer", "description": "Token lifetime in seconds."},
	}, "token", "tokenType", "expiresIn")
	result["RunnerTokenRequest"] = record(object{"runner": textSchema()}, "runner")
	result["RunnerTokenResponse"] = record(object{
		"accessToken": textSchema(), "tokenType": object{"type": "string", "enum": []string{"Bearer"}},
		"expiresIn": object{"type": "integer"},
	}, "accessToken", "tokenType", "expiresIn")
	result["CheckResponse"] = record(object{
		"authenticated": object{"type": "boolean"},
		"identity": record(object{
			"type": object{"type": "string", "enum": []string{"user", "runner"}}, "name": textSchema(),
			"scopes": object{"type": "array", "items": textSchema()},
		}, "type", "name", "scopes"),
		"expiresAt": object{"type": "string", "format": "date-time"},
	}, "authenticated", "identity", "expiresAt")
	result["PasswordRequest"] = record(object{
		"currentPassword": object{"type": "string", "format": "password"},
		"newPassword":     object{"type": "string", "format": "password", "description": "12–128 UTF-8 bytes."},
	}, "currentPassword", "newPassword")
	result["MachineRequest"] = record(object{
		"name":            textSchema(),
		"clientSecret":    object{"type": "string", "format": "password", "description": "Unpadded base64url; at least 32 decoded bytes, at most 256 encoded characters."},
		"tokenTTLSeconds": object{"type": "integer", "description": "0 or omitted: 3600; otherwise 300–86400 seconds."},
	}, "name", "clientSecret")
	for _, name := range []string{"LoginRequest", "RegisterRequest", "RunnerTokenRequest", "PasswordRequest", "MachineRequest", "JobAbort"} {
		result[name].(map[string]any)["additionalProperties"] = false
	}
	result["BuildCreate"].(map[string]any)["example"] = object{
		"apiVersion": "ebs/v1", "kind": "Build",
		"metadata": object{"name": "00000000-0000-4000-8000-000000000001", "namespace": "example"},
		"spec":     object{"buildType": "single", "buildTarget": object{"os": "openEuler:24.03-LTS-SP3", "arch": "x86_64"}, "packages": []string{"bash"}},
	}
	result["ScriptCreate"].(map[string]any)["example"] = object{
		"apiVersion": "ebs/v1", "kind": "Script", "metadata": object{"name": "example"},
		"spec": object{"content": "#!/bin/sh\nset -eu\necho build\n"},
	}
	return result
}

func requestSchemas(definitions object) {
	for _, name := range []string{"Project", "Runner", "Config", "Script", "Build", "Job", "User"} {
		group := "ebs"
		if name == "User" {
			group = "iam"
		}
		original := definitions[schemaName("ebs-api/"+group+"/v1."+name)].(map[string]any)
		properties := original["properties"].(map[string]any)
		for _, mode := range []string{"Create", "Update", "StatusUpdate"} {
			metadataFields := object{"name": textSchema(), "namespace": textSchema(),
				"labels":      object{"type": "object", "additionalProperties": textSchema()},
				"annotations": object{"type": "object", "additionalProperties": textSchema()}}
			requiredMetadata := []string{"name"}
			if mode != "Create" {
				requiredMetadata = append(requiredMetadata, "resourceVersion")
			}
			input := object{"apiVersion": object{"type": "string", "enum": []string{group + "/v1"}},
				"kind": object{"type": "string", "enum": []string{name}}, "metadata": record(metadataFields, requiredMetadata...)}
			if mode != "Create" {
				input["metadata"] = object{"allOf": []object{ref("k8s.io/apimachinery/pkg/apis/meta/v1.ObjectMeta")},
					"required": requiredMetadata, "description": "Copy the full metadata from GET, including UID, timestamps and resourceVersion. Preserve server-managed fields exactly."}
			}
			if group == "iam" {
				input["apiVersion"] = object{"type": "string", "enum": []string{"iam.ebs/v1"}}
			}
			required := []string{"apiVersion", "kind", "metadata"}
			if properties["spec"] != nil {
				input["spec"] = properties["spec"]
				required = append(required, "spec")
			}
			if mode == "StatusUpdate" {
				if properties["status"] == nil {
					continue
				}
				input["status"] = properties["status"]
				required = append(required, "status")
			}
			definitions[name+mode] = record(input, required...)
		}
	}
}

// Kubernetes metadata is outside the public resource generator's input packages.
func metadataSchema(t reflect.Type, definitions object) object {
	if t.Kind() == reflect.Pointer {
		return metadataSchema(t.Elem(), definitions)
	}
	if t.Name() == "Time" || t.Name() == "MicroTime" {
		return object{"type": "string", "format": "date-time"}
	}
	if t.Name() == "FieldsV1" {
		return object{"type": "object", "additionalProperties": true}
	}
	switch t.Kind() {
	case reflect.String:
		return textSchema()
	case reflect.Bool:
		return object{"type": "boolean"}
	case reflect.Int, reflect.Int32, reflect.Int64:
		return object{"type": "integer", "format": "int64"}
	case reflect.Slice:
		return object{"type": "array", "items": metadataSchema(t.Elem(), definitions)}
	case reflect.Map:
		return object{"type": "object", "additionalProperties": metadataSchema(t.Elem(), definitions)}
	case reflect.Struct:
		name := schemaName(t.PkgPath() + "." + t.Name())
		if _, exists := definitions[name]; exists {
			return ref(name)
		}
		properties := object{}
		schema := record(properties)
		definitions[name] = schema
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
			if field.Anonymous && jsonName == "" {
				for _, key := range []string{"kind", "apiVersion"} {
					properties[key] = textSchema()
				}
				continue
			}
			if jsonName != "" && jsonName != "-" {
				properties[jsonName] = metadataSchema(field.Type, definitions)
			}
		}
		return ref(name)
	default:
		return object{}
	}
}
