package openapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDocumentContract(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.0.3" {
		t.Fatalf("version=%v", doc["openapi"])
	}
	servers := doc["servers"].([]any)
	if len(servers) != 1 || servers[0].(map[string]any)["url"] != "/" {
		t.Fatalf("servers=%v", servers)
	}
	var checkRefs func(any)
	checkRefs = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if ref, ok := value["$ref"].(string); ok {
				if !strings.HasPrefix(ref, "#/components/") {
					t.Fatalf("nonlocal ref=%s", ref)
				}
				var target any = doc
				for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					mapping, ok := target.(map[string]any)
					if !ok {
						t.Fatalf("invalid ref=%s", ref)
					}
					target, ok = mapping[segment]
					if !ok {
						t.Fatalf("unresolved ref=%s", ref)
					}
				}
			}
			for _, child := range value {
				checkRefs(child)
			}
		case []any:
			for _, child := range value {
				checkRefs(child)
			}
		}
	}
	checkRefs(doc)
	ids := map[string]bool{}
	for path, value := range doc["paths"].(map[string]any) {
		if !strings.HasPrefix(path, "/auth/") && !strings.HasPrefix(path, "/apis/") {
			t.Fatalf("unexpected path=%s", path)
		}
		for method, operation := range value.(map[string]any) {
			op := operation.(map[string]any)
			id, _ := op["operationId"].(string)
			if id == "" || ids[id] {
				t.Fatalf("duplicate or empty operationId=%q", id)
			}
			ids[id] = true
			if len(op["responses"].(map[string]any)) == 0 || op["description"] == "" {
				t.Fatalf("incomplete %s %s", method, path)
			}
			if _, ok := op["security"].([]any); !ok {
				t.Fatalf("missing security for %s %s", method, path)
			}
			parameters, _ := op["parameters"].([]any)
			pathParameters := map[string]bool{}
			for _, value := range parameters {
				param := value.(map[string]any)
				if param["in"] == "path" && param["required"] == true {
					pathParameters[param["name"].(string)] = true
				}
			}
			for _, segment := range strings.Split(path, "/") {
				if strings.HasPrefix(segment, "{") && !pathParameters[strings.Trim(segment, "{} ")] {
					t.Fatalf("missing parameter in %s", path)
				}
			}
		}
	}
}

func TestTransformedResponseSchemas(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatal(err)
	}
	for name, fields := range map[string][]string{
		"ServiceEndpoints":    {"artifactManagers", "gitServer"},
		"LoginResponse":       {"token", "tokenType", "expiresIn"},
		"RunnerTokenResponse": {"accessToken", "tokenType", "expiresIn"},
		"CheckResponse":       {"authenticated", "identity", "expiresAt"},
	} {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(doc.Components.Schemas[name], &schema); err != nil {
			t.Fatal(err)
		}
		if len(schema.Properties) != len(fields) {
			t.Fatalf("%s fields=%v", name, schema.Properties)
		}
		for _, field := range fields {
			if schema.Properties[field] == nil {
				t.Fatalf("%s missing %s", name, field)
			}
		}
	}
}
