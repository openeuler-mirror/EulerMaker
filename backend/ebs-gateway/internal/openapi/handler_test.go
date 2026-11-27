package openapi

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
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
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if _, exists := schemas["JobUpdate"]; exists {
		t.Fatal("Job ordinary update schema is no longer exposed")
	}
	paths := doc["paths"].(map[string]any)
	if _, exists := schemas["JobCreate"]; exists {
		t.Fatal("Job create schema is still exposed")
	}
	if _, exists := paths["/apis/ebs/v1/projects/{project}/jobs"].(map[string]any)["post"]; exists {
		t.Fatal("Job creation is still exposed")
	}
	jobPath := "/apis/ebs/v1/projects/{project}/jobs/{name}"
	if _, exists := paths[jobPath].(map[string]any)["delete"]; exists {
		t.Fatal("Job deletion is still exposed")
	}
	for _, method := range []string{"put", "patch"} {
		if _, exists := paths[jobPath].(map[string]any)[method]; exists {
			t.Fatalf("Job ordinary %s is still exposed", method)
		}
		if _, exists := paths[jobPath+"/status"].(map[string]any)[method]; exists != (method == "patch") {
			t.Fatalf("Job status %s exposed=%v", method, exists)
		}
	}
	if _, exists := paths[jobPath+"/abort"].(map[string]any)["post"]; !exists {
		t.Fatal("Job abort is missing")
	}
	for _, name := range []string{"MachineRequest", "ebs-api.iam.v1.MachineAccountSpec"} {
		properties := schemas[name].(map[string]any)["properties"].(map[string]any)
		typ := properties["type"].(map[string]any)
		if !reflect.DeepEqual(typ["enum"], []any{"runner", "service"}) {
			t.Fatalf("%s type enum=%v", name, typ["enum"])
		}
		if name == "MachineRequest" && typ["default"] != "runner" {
			t.Fatalf("registration default type=%v", typ["default"])
		}
		projects := properties["projects"].(map[string]any)
		permissions := properties["permissions"].(map[string]any)
		if projects["type"] != "array" || projects["items"].(map[string]any)["type"] != "string" || permissions["type"] != "array" {
			t.Fatalf("%s grants schemas: projects=%v permissions=%v", name, projects, permissions)
		}
		enum := permissions["items"].(map[string]any)["enum"].([]any)
		sort.Slice(enum, func(i, j int) bool { return enum[i].(string) < enum[j].(string) })
		if !reflect.DeepEqual(enum, []any{"builds:abort", "builds:create", "projects:update"}) {
			t.Fatalf("%s permission enum=%v", name, enum)
		}
	}
	buildStatus := schemas["ebs-api.ebs.v1.BuildStatus"].(map[string]any)["properties"].(map[string]any)
	phases := buildStatus["phase"].(map[string]any)["enum"].([]any)
	wantPhases := ebsv1.BuildPhaseValues()
	if len(phases) != len(wantPhases) {
		t.Fatalf("Build phase enum=%v, want %v", phases, wantPhases)
	}
	for i, phase := range phases {
		if phase != wantPhases[i] {
			t.Fatalf("Build phase enum=%v, want %v", phases, wantPhases)
		}
	}
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
		"ServiceEndpoints":     {"artifactManagers", "gitServer"},
		"LoginResponse":        {"token", "tokenType", "expiresIn"},
		"MachineTokenResponse": {"accessToken", "tokenType", "expiresIn"},
		"CheckResponse":        {"authenticated", "identity", "expiresAt"},
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

func TestMachineTokenSchemas(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				OneOf []struct {
					Type                 string
					AdditionalProperties bool
					Required             []string
					MaxProperties        *int
					Properties           map[string]json.RawMessage
				}
				Properties map[string]json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatal(err)
	}
	variants := doc.Components.Schemas["MachineTokenRequest"].OneOf
	if len(variants) != 2 {
		t.Fatalf("machine token variants=%+v", variants)
	}
	runner, service := variants[0], variants[1]
	if runner.Type != "object" || runner.AdditionalProperties || len(runner.Required) != 1 || runner.Required[0] != "runner" || len(runner.Properties) != 1 {
		t.Fatalf("runner request=%+v", runner)
	}
	if service.Type != "object" || service.AdditionalProperties || service.MaxProperties == nil || *service.MaxProperties != 0 {
		t.Fatalf("service request=%+v", service)
	}
	var identity struct {
		Properties map[string]struct{ Enum []string }
	}
	if err := json.Unmarshal(doc.Components.Schemas["CheckResponse"].Properties["identity"], &identity); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(identity.Properties["type"].Enum, ","); got != "user,runner,service" {
		t.Fatalf("identity types=%s", got)
	}
}

func TestProjectDeleteNotExposed(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatal(err)
	}
	path := doc.Paths["/apis/ebs/v1/projects/{project}"]
	if _, ok := path["delete"]; ok {
		t.Fatal("Project DELETE is exposed")
	}
	for _, method := range []string{"get", "put", "patch"} {
		if _, ok := path[method]; !ok {
			t.Fatalf("Project %s missing", method)
		}
	}
}
