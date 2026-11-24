package policy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
	"ebs-gateway/internal/upstream"
)

func TestProjectAccessProjectionAndErrors(t *testing.T) {
	const metadata = `{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice"}}}`
	for _, tc := range []struct {
		name           string
		status         int
		body           string
		transportError bool
		want           int
	}{
		{"large project projected", 200, metadata, false, 0},
		{"missing", 404, "", false, 404},
		{"unavailable", 503, "private upstream error", false, 503},
		{"throttled", 429, "", false, 503},
		{"invalid JSON", 200, "{", false, 502},
		{"wrong identity", 200, `{"metadata":{"name":"other"}}`, false, 502},
		{"connection error", 0, "", true, 503},
		{"not owner", 200, `{"metadata":{"name":"team"}}`, false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := upstream.New("https://api.example", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("includeFields") != "metadata.name,metadata.labels" {
					t.Error("Project lookup must request metadata only")
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
						`{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice"}},"spec":{"large":"` + strings.Repeat("x", 2<<20) + `"}}`))}, nil
				}
				if tc.transportError {
					return nil, errors.New("private connection error")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			a := New(client)
			who := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}
			for _, route := range []Route{
				{Resource: "projects", Name: "team", Method: http.MethodPatch},
				{Resource: "builds", Project: "team", Name: "b", Method: http.MethodDelete},
				{Resource: "builds", Project: "team", Method: http.MethodPost},
				{Resource: "jobs", Project: "team", Name: "j", Subresource: "abort", Method: http.MethodPost},
			} {
				err := a.Authorize(context.Background(), who, route)
				if tc.want == 0 {
					if err != nil {
						t.Fatal(err)
					}
					continue
				}
				var classified *Error
				if !errors.As(err, &classified) || classified.Status != tc.want {
					t.Fatalf("route=%+v error=%v, want status %d", route, err, tc.want)
				}
				if strings.Contains(err.Error(), "private") {
					t.Fatal("upstream details exposed")
				}
			}
		})
	}
}

func projectAuthorizer(t *testing.T) *Authorizer {
	t.Helper()
	client, err := upstream.New("https://api.example", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/apis/ebs/v1/projects/team" {
			t.Fatalf("unexpected Project lookup: %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice","ebs.io/member-user.bob":"true"}}}`))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return New(client)
}

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
				{"missing owner", `{"metadata":{"name":"team","labels":{"ebs.io/project-type":"personal"}}}`},
				{"different owner", `{"metadata":{"name":"team","labels":{"ebs.io/project-type":"personal","ebs.io/owner-user":"bob"}}}`},
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
		[]byte(`{"metadata":{"name":"team","labels":{"ebs.io/project-type":"community"}}}`))
	if err == nil {
		t.Fatal("community Project creation was allowed")
	}
}

func TestProjectPurposeRequiresPrivilegedWriter(t *testing.T) {
	for _, scope := range []identity.Scope{identity.UserScope, identity.OpsScope, identity.AdminScope} {
		for _, before := range []string{"", "gating"} {
			for _, after := range []string{"", "gating", "other"} {
				oldLabels := map[string]any{ebsv1.ProjectOwnerUserLabel: "alice"}
				newLabels := map[string]any{ebsv1.ProjectOwnerUserLabel: "alice"}
				if before != "" {
					oldLabels[ebsv1.BuildPurposeLabel] = before
				}
				if after != "" {
					newLabels[ebsv1.BuildPurposeLabel] = after
				}
				old := mutation.Object{"metadata": map[string]any{"name": "team", "labels": oldLabels}}
				next := mutation.Object{"metadata": map[string]any{"name": "team", "labels": newLabels}}
				who := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: scope}
				err := (&Authorizer{}).ValidateUpdate(context.Background(), who, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, old, next)
				if (err == nil) != (who.IsPrivileged() || before == after) {
					t.Fatalf("scope=%s %q -> %q: %v", scope, before, after, err)
				}
			}
		}
	}
}

func TestProjectPurposeOwnerPermissions(t *testing.T) {
	a := projectAuthorizer(t)
	for _, scope := range []identity.Scope{identity.UserScope, identity.OpsScope, identity.AdminScope} {
		for _, subject := range []string{"alice", "bob", "outsider"} {
			for _, method := range []string{http.MethodPut, http.MethodPatch} {
				for _, extra := range []string{"", "spec", "annotations", "labels"} {
					var old, next mutation.Object
					body := []byte(`{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice"}},"spec":{"description":"original"}}`)
					if err := json.Unmarshal(body, &old); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(body, &next); err != nil {
						t.Fatal(err)
					}
					meta := objectMap(next["metadata"])
					objectMap(meta["labels"])[ebsv1.BuildPurposeLabel] = ebsv1.BuildPurposeGating
					switch extra {
					case "spec":
						next["spec"] = map[string]any{"description": "changed"}
					case "annotations":
						meta["annotations"] = map[string]any{"note": "changed"}
					case "labels":
						objectMap(meta["labels"])["note"] = "changed"
					}
					who := identity.Principal{Type: identity.UserType, Subject: subject, Scope: scope}
					route := Route{Resource: "projects", Name: "team", Method: method}
					err := a.Authorize(context.Background(), who, route)
					if err == nil {
						err = a.ValidateUpdate(context.Background(), who, route, old, next)
					}
					want := subject == "alice" && who.IsPrivileged() && (scope == identity.OpsScope || extra == "")
					if (err == nil) != want {
						t.Fatalf("scope=%s subject=%s method=%s extra=%s: %v, want allowed=%t", scope, subject, method, extra, err, want)
					}
				}
			}
		}
	}
}

func TestOrdinaryUserMayOnlyBackfillPersonalProjectType(t *testing.T) {
	authorizer := &Authorizer{}
	old := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Project",
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{ebsv1.ProjectOwnerUserLabel: "alice"}},
		"spec":     map[string]any{}, "status": map[string]any{},
	}
	personal := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Project",
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{ebsv1.ProjectOwnerUserLabel: "alice", ebsv1.ProjectTypeLabel: "personal"}},
		"spec":     map[string]any{}, "status": map[string]any{},
	}
	community := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Project",
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{ebsv1.ProjectOwnerUserLabel: "alice", ebsv1.ProjectTypeLabel: "community"}},
		"spec":     map[string]any{}, "status": map[string]any{},
	}
	who := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}
	route := Route{Resource: "projects", Name: "team", Method: http.MethodPut}
	if err := authorizer.ValidateUpdate(context.Background(), who, route, old, personal); err != nil {
		t.Fatalf("personal backfill was rejected: %v", err)
	}
	if err := authorizer.ValidateUpdate(context.Background(), who, route, old, community); err == nil {
		t.Fatal("community change was allowed")
	}
}

func TestProjectOwnerIsImmutable(t *testing.T) {
	old := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Project",
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{ebsv1.ProjectOwnerUserLabel: "alice"}},
		"spec":     map[string]any{}, "status": map[string]any{},
	}
	candidate := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Project",
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{ebsv1.ProjectOwnerUserLabel: "bob"}},
		"spec":     map[string]any{}, "status": map[string]any{},
	}
	who := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}
	if err := (&Authorizer{}).ValidateUpdate(context.Background(), who,
		Route{Resource: "projects", Name: "team", Method: http.MethodPut}, old, candidate); err == nil {
		t.Error("Project owner was allowed to transfer ownership")
	}
}

func TestProjectOrdinaryUpdateDelegatesStatusToAPIServer(t *testing.T) {
	old := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Project",
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{ebsv1.ProjectOwnerUserLabel: "alice"}},
		"spec":     map[string]any{}, "status": map[string]any{"phase": "Active"},
	}
	who := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}
	route := Route{Resource: "projects", Name: "team", Method: http.MethodPut}
	for _, withStatus := range []bool{false, true} {
		candidate := mutation.Object{
			"apiVersion": old["apiVersion"], "kind": old["kind"], "metadata": old["metadata"],
			"spec": map[string]any{"description": "updated"},
		}
		if withStatus {
			candidate["status"] = map[string]any{"phase": "Terminating"}
		}
		if err := (&Authorizer{}).ValidateUpdate(context.Background(), who, route, old, candidate); err != nil {
			t.Fatalf("ordinary update with status=%v was rejected: %v", withStatus, err)
		}
	}
}
