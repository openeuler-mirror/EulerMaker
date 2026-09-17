package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectTypeCreate(t *testing.T) {
	g := &Gateway{}
	for _, scope := range []string{"ebs:user", "ebs:ops"} {
		for _, labels := range []string{`{}`, `{"project.ebs.io/type":"personal"}`, `{"project.ebs.io/type":"community"}`} {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"metadata":{"labels":`+labels+`}}`))
			_, err := g.handleProjectCollection(context.Background(), r, Identity{Subject: "alice", Scopes: []string{scope}})
			denied := scope == "ebs:user" && strings.Contains(labels, "community")
			if (err != nil) != denied {
				t.Fatalf("%s %s: %v", scope, labels, err)
			}
		}
	}
}

func TestLegacyProjectType(t *testing.T) {
	for _, tc := range []struct {
		labels  map[string]any
		allowed bool
	}{
		{nil, true},
		{map[string]any{projectTypeLabel: "personal"}, true},
		{map[string]any{projectTypeLabel: "community"}, false},
		{map[string]any{projectTypeLabel: nil}, false},
		{map[string]any{projectTypeLabel: ""}, false},
	} {
		if sameProjectType(tc.labels, nil) != tc.allowed {
			t.Fatalf("unexpected permission for legacy project: %v", tc.labels)
		}
	}
}

func TestProjectTypeWrites(t *testing.T) {
	g := &Gateway{cfg: Config{MaxRequestBodyBytes: 1024 * 1024}}
	old, _ := projectFromAny(map[string]any{"metadata": map[string]any{
		"name": "p", "resourceVersion": "7", "labels": map[string]any{ownerUserLabel: "alice", projectTypeLabel: "community"},
	}})
	for _, tc := range []struct {
		name, method, contentType, body string
		denied                          bool
	}{
		{"put unchanged", "PUT", "application/json", `{"metadata":{"resourceVersion":"7","labels":{"ebs.io/owner-user":"alice","project.ebs.io/type":"community"}}}`, false},
		{"put stale version", "PUT", "application/json", `{"metadata":{"resourceVersion":"8","labels":{"ebs.io/owner-user":"alice","project.ebs.io/type":"community"}}}`, true},
		{"put changed", "PUT", "application/json", `{"metadata":{"labels":{"ebs.io/owner-user":"alice","project.ebs.io/type":"personal"}}}`, true},
		{"put removed", "PUT", "application/json", `{"metadata":{"labels":{"ebs.io/owner-user":"alice"}}}`, true},
		{"merge spec", "PATCH", "application/merge-patch+json", `{"spec":{"displayName":"new"}}`, false},
		{"merge remove", "PATCH", "application/merge-patch+json", `{"metadata":{"labels":{"project.ebs.io/type":null}}}`, true},
		{"merge labels null", "PATCH", "application/merge-patch+json", `{"metadata":{"labels":null}}`, true},
		{"json remove", "PATCH", "application/json-patch+json", `[{"op":"remove","path":"/metadata/labels/project.ebs.io~1type"}]`, true},
		{"json replace labels", "PATCH", "application/json-patch+json", `[{"op":"replace","path":"/metadata/labels","value":{"ebs.io/owner-user":"alice"}}]`, true},
		{"json move", "PATCH", "application/json-patch+json", `[{"op":"move","from":"/metadata/labels/project.ebs.io~1type","path":"/metadata/labels/other"}]`, true},
		{"json copy", "PATCH", "application/json-patch+json", `[{"op":"copy","from":"/metadata/name","path":"/metadata/labels/project.ebs.io~1type"}]`, true},
		{"version changed", "PATCH", "application/merge-patch+json", `{"metadata":{"resourceVersion":"8"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			err := g.protectProjectAccessLabels(r, Identity{Subject: "alice", Scopes: []string{"ebs:user"}}, old)
			if (err != nil) != tc.denied {
				t.Fatalf("denied=%v: %v", tc.denied, err)
			}
			if !tc.denied && tc.method == "PATCH" && r.Method != "PUT" {
				t.Fatal("patch must become version-pinned PUT")
			}
		})
	}
	r := httptest.NewRequest("PUT", "/", strings.NewReader(`{"metadata":{"labels":{"ebs.io/owner-user":"alice","project.ebs.io/type":"personal"}}}`))
	if err := g.protectProjectAccessLabels(r, Identity{Subject: "alice", Scopes: []string{"ebs:ops"}}, old); err != nil {
		t.Fatal(err)
	}
}
