package policy

import (
	"context"
	"net/http"
	"testing"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

func TestOrdinaryUserCannotCreateCommunityProject(t *testing.T) {
	authorizer := &Authorizer{}
	_, err := authorizer.PrepareCreate(context.Background(), identity.Principal{Subject: "alice", Scope: identity.UserScope},
		Route{Resource: "projects", Method: http.MethodPost},
		[]byte(`{"metadata":{"name":"team","labels":{"project.ebs.io/type":"community"}}}`))
	if err == nil {
		t.Fatal("community Project creation was allowed")
	}
}

func TestProjectScopedCreateCannotNameAnotherNamespace(t *testing.T) {
	authorizer := &Authorizer{}
	_, err := authorizer.PrepareCreate(context.Background(), identity.Principal{Subject: "alice", Scope: identity.UserScope},
		Route{Resource: "jobs", Project: "team", Method: http.MethodPost},
		[]byte(`{"metadata":{"name":"job-1","namespace":"other"}}`))
	if err == nil {
		t.Fatal("cross-Project create was allowed")
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
	who := identity.Principal{Subject: "alice", Scope: identity.UserScope}
	route := Route{Resource: "projects", Name: "team", Method: http.MethodPut}
	if err := authorizer.ValidateUpdate(context.Background(), who, route, old, personal); err != nil {
		t.Fatalf("personal backfill was rejected: %v", err)
	}
	if err := authorizer.ValidateUpdate(context.Background(), who, route, old, community); err == nil {
		t.Fatal("community change was allowed")
	}
}

func TestProjectOwnerCannotBeChangedByAdminOrSystem(t *testing.T) {
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
	for _, scope := range []identity.Scope{identity.AdminScope, identity.SystemScope} {
		who := identity.Principal{Subject: string(scope), Scope: scope}
		if err := (&Authorizer{}).ValidateUpdate(context.Background(), who,
			Route{Resource: "projects", Name: "team", Method: http.MethodPut}, old, candidate); err == nil {
			t.Errorf("%s changed Project owner", scope)
		}
	}
}
