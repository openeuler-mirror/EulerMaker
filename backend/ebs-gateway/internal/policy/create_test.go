package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"ebs-gateway/internal/identity"
)

func TestProjectScopedCreateCannotNameAnotherNamespace(t *testing.T) {
	authorizer := &Authorizer{}
	_, err := authorizer.PrepareCreate(context.Background(), identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope},
		Route{Resource: "jobs", Project: "team", Method: http.MethodPost},
		[]byte(`{"metadata":{"name":"job-1","namespace":"other"}}`))
	if err == nil {
		t.Fatal("cross-Project create was allowed")
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
					if got := object.Metadata.Labels["ebs.io/owner-user"]; got != who.Subject {
						t.Fatalf("owner = %q, want %q", got, who.Subject)
					}
					if input.name != "missing labels" && object.Metadata.Labels["project.ebs.io/type"] != "personal" {
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
