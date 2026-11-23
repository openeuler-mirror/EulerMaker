package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

func TestProjectPurposeRequiresPrivilegedWriter(t *testing.T) {
	for _, scope := range []identity.Scope{identity.UserScope, identity.OpsScope, identity.AdminScope} {
		for _, before := range []string{"", "gating"} {
			for _, after := range []string{"", "gating", "other"} {
				oldLabels := map[string]any{"ebs.io/owner-user": "alice"}
				newLabels := map[string]any{"ebs.io/owner-user": "alice"}
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
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{"ebs.io/owner-user": "alice"}},
		"spec":     map[string]any{}, "status": map[string]any{},
	}
	personal := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Project",
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{"ebs.io/owner-user": "alice", "project.ebs.io/type": "personal"}},
		"spec":     map[string]any{}, "status": map[string]any{},
	}
	community := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Project",
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{"ebs.io/owner-user": "alice", "project.ebs.io/type": "community"}},
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
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{"ebs.io/owner-user": "alice"}},
		"spec":     map[string]any{}, "status": map[string]any{},
	}
	candidate := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Project",
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{"ebs.io/owner-user": "bob"}},
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
		"metadata": map[string]any{"name": "team", "resourceVersion": "1", "labels": map[string]any{"ebs.io/owner-user": "alice"}},
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
