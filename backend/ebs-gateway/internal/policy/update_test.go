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

func TestRunnerTaintsAreManagedAfterRegistration(t *testing.T) {
	taints := []any{map[string]any{"key": "pool", "value": "gating", "effect": "NoSchedule"}}
	for _, tc := range []struct {
		name             string
		oldSpec, newSpec map[string]any
		allowed          bool
	}{
		{"preserve", map[string]any{"type": "ct", "taints": taints}, map[string]any{"type": "vm", "taints": taints}, true},
		{"add", map[string]any{"type": "ct"}, map[string]any{"type": "ct", "taints": taints}, false},
		{"clear", map[string]any{"taints": taints}, map[string]any{"taints": []any{}}, false},
		{"remove", map[string]any{"taints": taints}, map[string]any{}, false},
		{"change", map[string]any{"taints": taints}, map[string]any{"taints": []any{map[string]any{"key": "pool", "effect": "NoExecute"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := mutation.Object{"metadata": map[string]any{"name": "runner-a"}, "spec": tc.oldSpec}
			next := mutation.Object{"metadata": map[string]any{"name": "runner-a"}, "spec": tc.newSpec}
			err := (&Authorizer{}).ValidateUpdate(context.Background(),
				identity.Principal{Type: identity.RunnerType, Subject: "runner-a", Runner: "runner-a"},
				Route{Resource: "runners", Name: "runner-a", Method: http.MethodPatch}, old, next)
			if (err == nil) != tc.allowed {
				t.Fatalf("ValidateUpdate() = %v, allowed=%v", err, tc.allowed)
			}
		})
	}
}

func TestRunnerJobStatusUpdateWhitelist(t *testing.T) {
	who := identity.Principal{Type: identity.RunnerType, Subject: "runner-1", Runner: "runner-1"}
	route := Route{Resource: "jobs", Project: "team", Name: "job-1", Subresource: "status", Method: http.MethodPatch}
	old := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Job",
		"metadata": map[string]any{"name": "job-1", "namespace": "team", "resourceVersion": "1"},
		"spec":     map[string]any{"command": "build"},
		"status":   map[string]any{"runner": "runner-1", "phase": "Running"},
	}
	allowed := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Job",
		"metadata": map[string]any{"name": "job-1", "namespace": "team", "resourceVersion": "1"},
		"spec":     map[string]any{"command": "build"},
		"status":   map[string]any{"runner": "runner-1", "phase": "Succeeded"},
	}
	if err := (&Authorizer{}).ValidateUpdate(context.Background(), who, route, old, allowed); err != nil {
		t.Fatalf("allowed phase update was rejected: %v", err)
	}
	postRun := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Job",
		"metadata": map[string]any{"name": "job-1", "namespace": "team", "resourceVersion": "1"},
		"spec":     map[string]any{"command": "build"},
		"status": map[string]any{
			"runner": "runner-1", "phase": "Running", "stage": "PostRun",
			"result": map[string]any{"status": "InstallFailed", "missingDeps": map[string]any{"missing": map[string]any{}}},
		},
	}
	if err := (&Authorizer{}).ValidateUpdate(context.Background(), who, route, old, postRun); err != nil {
		t.Fatalf("first build/install result write was rejected: %v", err)
	}
	updatedResult := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Job",
		"metadata": map[string]any{"name": "job-1", "namespace": "team", "resourceVersion": "1"},
		"spec":     map[string]any{"command": "build"},
		"status": map[string]any{
			"runner": "runner-1", "phase": "Succeeded", "stage": "PostRun",
			"result": map[string]any{"status": "Succeeded"},
		},
	}
	if err := (&Authorizer{}).ValidateUpdate(context.Background(), who, route, postRun, updatedResult); err != nil {
		t.Fatalf("subsequent install result update was rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		status map[string]any
	}{
		{"runner reassignment", map[string]any{"runner": "runner-2", "phase": "Succeeded"}},
		{"new protected field", map[string]any{"runner": "runner-1", "phase": "Succeeded", "secret": "x"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := mutation.Object{
				"apiVersion": "ebs/v1", "kind": "Job",
				"metadata": map[string]any{"name": "job-1", "namespace": "team", "resourceVersion": "1"},
				"spec":     map[string]any{"command": "build"},
				"status":   test.status,
			}
			if err := (&Authorizer{}).ValidateUpdate(context.Background(), who, route, old, candidate); err == nil {
				t.Fatal("protected Job status change was allowed")
			}
		})
	}
}

func TestUpdateVersionAndStatusBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, resource, subresource string
		change                      func(mutation.Object)
		want                        int
	}{
		{"runner status version", "runners", "status", func(o mutation.Object) { objectMap(o["metadata"])["resourceVersion"] = "2" }, 0},
		{"runner ordinary version", "runners", "", func(o mutation.Object) { objectMap(o["metadata"])["resourceVersion"] = "2" }, 0},
		{"job status version", "jobs", "status", func(o mutation.Object) { objectMap(o["metadata"])["resourceVersion"] = "2" }, http.StatusForbidden},
		{"machine version before permission", "machineaccounts", "", func(o mutation.Object) { objectMap(o["metadata"])["resourceVersion"] = "2" }, http.StatusConflict},
		{"machine UID before permission", "machineaccounts", "", func(o mutation.Object) { objectMap(o["metadata"])["uid"] = "new" }, http.StatusConflict},
		{"status spec protection", "runners", "status", func(o mutation.Object) { o["spec"] = map[string]any{"type": "vm"} }, http.StatusForbidden},
		{"status metadata protection", "runners", "status", func(o mutation.Object) { objectMap(o["metadata"])["labels"] = map[string]any{"pool": "new"} }, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := mutation.Object{
				"metadata": map[string]any{"name": "worker", "uid": "id", "resourceVersion": "1"},
				"spec":     map[string]any{"type": "ct"},
				"status":   map[string]any{"phase": "Online"},
			}
			raw, _ := json.Marshal(old)
			candidate, err := mutation.ParseObject(raw)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(candidate)
			who := identity.Principal{Type: identity.RunnerType, Subject: "worker", Runner: "worker"}
			route := Route{Resource: tc.resource, Name: "worker", Subresource: tc.subresource, Method: http.MethodPatch}
			err = (&Authorizer{}).ValidateUpdate(context.Background(), who, route, old, candidate)
			if tc.want == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else if denied, ok := err.(*Error); !ok || denied.Status != tc.want {
				t.Fatalf("error=%v, want status=%d", err, tc.want)
			}
		})
	}
}

func TestRunnerActiveJobsStatusUpdate(t *testing.T) {
	who := identity.Principal{Type: identity.RunnerType, Subject: "runner-1", Runner: "runner-1"}
	route := Route{Resource: "runners", Name: "runner-1", Subresource: "status", Method: http.MethodPatch}
	for _, count := range []int{2, 0} {
		old := mutation.Object{"status": map[string]any{"phase": "Online", "activeJobs": 1}}
		candidate := mutation.Object{"status": map[string]any{"phase": "Online", "activeJobs": count}}
		if err := validateStatusUpdate(who, route, old, candidate); err != nil {
			t.Fatalf("activeJobs=%d rejected: %v", count, err)
		}
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
