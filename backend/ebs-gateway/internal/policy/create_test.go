package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"ebs-gateway/internal/identity"
)

func TestProjectScopedCreateNamespace(t *testing.T) {
	for _, namespace := range []string{"", "team", "other"} {
		t.Run(namespace, func(t *testing.T) {
			meta := map[string]string{"name": "build-1"}
			if namespace != "" {
				meta["namespace"] = namespace
			}
			body, _ := json.Marshal(map[string]any{"metadata": meta})
			encoded, err := (&Authorizer{}).PrepareCreate(context.Background(), identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope},
				Route{Resource: "builds", Project: "team", Method: http.MethodPost}, body)
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
