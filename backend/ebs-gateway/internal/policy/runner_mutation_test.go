package policy

import (
	"context"
	"net/http"
	"testing"

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
