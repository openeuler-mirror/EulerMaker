package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

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
