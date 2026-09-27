package policy

import (
	"context"
	"net/http"
	"testing"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

func TestRunnerJobStatusUpdateWhitelist(t *testing.T) {
	who := identity.Principal{Type: identity.RunnerType, Subject: "runner-1", Runner: "runner-1"}
	route := Route{Resource: "jobs", Project: "team", Name: "job-1", Subresource: "status", Method: http.MethodPatch}
	old := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Job",
		"metadata": map[string]any{"name": "job-1", "namespace": "team", "resourceVersion": "1"},
		"spec":     map[string]any{"command": "build"},
		"status":   map[string]any{"runner": "runner-1", "phase": "Running", "restartCount": 0},
	}
	allowed := mutation.Object{
		"apiVersion": "ebs/v1", "kind": "Job",
		"metadata": map[string]any{"name": "job-1", "namespace": "team", "resourceVersion": "1"},
		"spec":     map[string]any{"command": "build"},
		"status":   map[string]any{"runner": "runner-1", "phase": "Succeeded", "restartCount": 0},
	}
	if err := (&Authorizer{}).ValidateUpdate(context.Background(), who, route, old, allowed); err != nil {
		t.Fatalf("allowed phase update was rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		status map[string]any
	}{
		{"runner reassignment", map[string]any{"runner": "runner-2", "phase": "Succeeded", "restartCount": 0}},
		{"restart count", map[string]any{"runner": "runner-1", "phase": "Succeeded", "restartCount": 1}},
		{"new protected field", map[string]any{"runner": "runner-1", "phase": "Succeeded", "restartCount": 0, "secret": "x"}},
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
