package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

func TestRunnerJobStatusUpdateWhitelist(t *testing.T) {
	who := identity.Principal{Type: identity.RunnerType, Subject: "runner-1"}
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
		{"runner ordinary version", "runners", "", func(o mutation.Object) { objectMap(o["metadata"])["resourceVersion"] = "2" }, 0},
		{"job status version", "jobs", "status", func(o mutation.Object) { objectMap(o["metadata"])["resourceVersion"] = "2" }, http.StatusForbidden},
		{"status spec protection", "jobs", "status", func(o mutation.Object) { o["spec"] = map[string]any{"type": "vm"} }, http.StatusForbidden},
		{"status metadata protection", "jobs", "status", func(o mutation.Object) { objectMap(o["metadata"])["labels"] = map[string]any{"pool": "new"} }, http.StatusForbidden},
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
			who := identity.Principal{Type: identity.RunnerType, Subject: "worker"}
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
