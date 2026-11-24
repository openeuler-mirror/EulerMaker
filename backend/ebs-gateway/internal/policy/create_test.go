package policy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

func TestProjectCreateBuildPurpose(t *testing.T) {
	for _, scope := range []identity.Scope{identity.UserScope, identity.OpsScope, identity.AdminScope} {
		for _, purpose := range []any{nil, "", "gating"} {
			who := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: scope}
			body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "team", "labels": map[string]any{ebsv1.BuildPurposeLabel: purpose}}})
			_, err := (&Authorizer{}).PrepareCreate(context.Background(), who, Route{Resource: "projects", Method: http.MethodPost}, body)
			if (err == nil) != who.IsPrivileged() {
				t.Fatalf("scope=%s purpose=%v error=%v", scope, purpose, err)
			}
		}
	}
}

func TestRunnerCreateWithInitialTaints(t *testing.T) {
	who := identity.Principal{Type: identity.RunnerType, Subject: "runner-a", Runner: "runner-a"}
	body := []byte(`{"metadata":{"name":"runner-a"},"spec":{"instanceID":"01234567-89ab-4def-8123-456789abcdef","type":"ct","arch":"x86_64","taints":[{"key":"pool","value":"gating","effect":"NoSchedule"}]}}`)
	encoded, err := (&Authorizer{}).PrepareCreate(context.Background(), who,
		Route{Resource: "runners", Method: http.MethodPost}, body)
	if err != nil {
		t.Fatal(err)
	}
	var object struct {
		Spec struct {
			Taints []struct{ Key, Value, Effect string }
		}
	}
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	if len(object.Spec.Taints) != 1 || object.Spec.Taints[0].Key != "pool" ||
		object.Spec.Taints[0].Value != "gating" || object.Spec.Taints[0].Effect != "NoSchedule" {
		t.Fatalf("initial taints were not preserved: %s", encoded)
	}
}

func TestProjectScopedCreateNamespace(t *testing.T) {
	for _, namespace := range []string{"", "team", "other"} {
		t.Run(namespace, func(t *testing.T) {
			meta := map[string]string{"name": "job-1"}
			if namespace != "" {
				meta["namespace"] = namespace
			}
			body, _ := json.Marshal(map[string]any{"metadata": meta})
			encoded, err := (&Authorizer{}).PrepareCreate(context.Background(), identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope},
				Route{Resource: "jobs", Project: "team", Method: http.MethodPost}, body)
			if namespace == "other" {
				if rejected, ok := err.(*Error); !ok || rejected.Status != http.StatusBadRequest {
					t.Fatalf("namespace mismatch error=%v", err)
				}
			} else if err != nil || string(encoded) != string(body) {
				t.Fatalf("create changed unrelated content: %s, err=%v", encoded, err)
			}
		})
	}
}

func TestProjectCreateValidatesAndPreservesMembers(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		client, err := upstream.New("https://api.example", roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/apis/iam.ebs/v1/users/bob" {
				t.Fatalf("unexpected member lookup: %s", request.URL.Path)
			}
			body, _ := json.Marshal(map[string]any{
				"metadata": map[string]any{"name": "bob"},
				"spec":     map[string]any{"enabled": enabled, "scopes": []string{"ebs:user"}},
			})
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		who := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}
		body := []byte(`{"metadata":{"name":"team","labels":{"ebs.io/member-user.bob":"true","custom":"keep"}},"spec":{"distro":"openEuler"}}`)
		encoded, err := New(client).PrepareCreate(context.Background(), who, Route{Resource: "projects", Method: http.MethodPost}, body)
		if !enabled {
			if rejected, ok := err.(*Error); !ok || rejected.Status != http.StatusForbidden {
				t.Fatalf("disabled member error=%v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		labels := objectMap(objectMap(object["metadata"])["labels"])
		if labels[ebsv1.ProjectOwnerUserLabel] != "alice" || labels[ebsv1.ProjectMemberUserLabelPrefix+"bob"] != "true" || labels["custom"] != "keep" || objectMap(object["spec"])["distro"] != "openEuler" {
			t.Fatalf("unexpected Project: %s", encoded)
		}
	}
}

func TestProjectCreateUsesTokenOwner(t *testing.T) {
	for _, scope := range []identity.Scope{identity.UserScope, identity.OpsScope, identity.AdminScope} {
		t.Run(string(scope), func(t *testing.T) {
			for _, input := range []struct {
				name string
				body string
			}{
				{"missing labels", `{"metadata":{"name":"team"}}`},
				{"missing owner", `{"metadata":{"name":"team","labels":{"project.ebs.io/type":"personal"}}}`},
				{"different owner", `{"metadata":{"name":"team","labels":{"project.ebs.io/type":"personal","ebs.io/owner-user":"bob"}}}`},
			} {
				t.Run(input.name, func(t *testing.T) {
					who := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: scope}
					encoded, err := (&Authorizer{}).PrepareCreate(context.Background(), who,
						Route{Resource: "projects", Method: http.MethodPost}, []byte(input.body))
					if err != nil {
						t.Fatal(err)
					}
					var object struct {
						Metadata struct {
							Labels map[string]string `json:"labels"`
						} `json:"metadata"`
					}
					if err := json.Unmarshal(encoded, &object); err != nil {
						t.Fatal(err)
					}
					if got := object.Metadata.Labels[ebsv1.ProjectOwnerUserLabel]; got != who.Subject {
						t.Fatalf("owner = %q, want %q", got, who.Subject)
					}
					if input.name != "missing labels" && object.Metadata.Labels[ebsv1.ProjectTypeLabel] != "personal" {
						t.Fatal("Project type label was not preserved")
					}
				})
			}
		})
	}
}

func TestOrdinaryUserCannotCreateCommunityProject(t *testing.T) {
	authorizer := &Authorizer{}
	_, err := authorizer.PrepareCreate(context.Background(), identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope},
		Route{Resource: "projects", Method: http.MethodPost},
		[]byte(`{"metadata":{"name":"team","labels":{"project.ebs.io/type":"community"}}}`))
	if err == nil {
		t.Fatal("community Project creation was allowed")
	}
}
