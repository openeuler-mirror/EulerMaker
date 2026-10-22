package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"ebs-runner/pkg/gateway"
)

func TestResolveArtifactRepositories(t *testing.T) {
	routes := NewRoutes("internal", &staticTokens{token: "token"}, http.DefaultClient)
	if err := routes.Update(gateway.ServiceEndpoints{ArtifactManagers: map[string]gateway.ServiceEndpoint{
		"default": {RunnerInternalURL: "http://default:8081"},
		"art-01":  {RunnerInternalURL: "http://art-01-old:8081"},
		"art-02":  {RunnerInternalURL: "http://art-02:8081"},
	}}); err != nil {
		t.Fatal(err)
	}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job", Annotations: map[string]string{
		"ebs.io/artifacts": "art-01",
	}}, Spec: gateway.JobSpec{Payload: `{"repo":[{"url":"artifact://art-01/repositories/v1/one/","priority":10},{"url":"artifact://art-02/repositories/v1/two/","priority":20},{"url":"artifact:///repositories/v1/three/","priority":30},{"url":"https://bootstrap.example/repo/","priority":99}],"commit_id":"abc"}`}}
	if err := routes.Pin(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := routes.Update(gateway.ServiceEndpoints{ArtifactManagers: map[string]gateway.ServiceEndpoint{
		"default": {RunnerInternalURL: "http://default:8081"},
		"art-01":  {RunnerInternalURL: "http://art-01-new:8081"},
		"art-02":  {RunnerInternalURL: "http://art-02:8081"},
	}}); err != nil {
		t.Fatal(err)
	}
	resolved, err := routes.ResolvePayload(job)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Repo []struct {
			URL      string `json:"url"`
			Priority int    `json:"priority"`
		} `json:"repo"`
		CommitID string `json:"commit_id"`
	}
	if err := json.Unmarshal([]byte(resolved), &payload); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"http://art-01-old:8081/repositories/v1/one/",
		"http://art-02:8081/repositories/v1/two/",
		"http://default:8081/repositories/v1/three/",
		"https://bootstrap.example/repo/",
	}
	for i, repo := range payload.Repo {
		if repo.URL != want[i] || repo.Priority != []int{10, 20, 30, 99}[i] {
			t.Fatalf("repo[%d] = %+v, want %s", i, repo, want[i])
		}
	}
	if payload.CommitID != "abc" {
		t.Fatalf("commit_id = %q", payload.CommitID)
	}
}

func TestResolveArtifactRepositoryExternal(t *testing.T) {
	routes := NewRoutes("external", &staticTokens{token: "token"}, http.DefaultClient)
	if err := routes.Update(gateway.ServiceEndpoints{ArtifactManagers: map[string]gateway.ServiceEndpoint{
		"art-01": {RunnerInternalURL: "http://internal:8081", RunnerExternalURL: "https://external.example/art-01"},
	}}); err != nil {
		t.Fatal(err)
	}
	job := gateway.JobResource{Spec: gateway.JobSpec{Payload: `{"repo":[{"url":"artifact://art-01/repositories/v1/repo/","priority":10}]}`}}
	resolved, err := routes.ResolvePayload(job)
	if err != nil || !strings.Contains(resolved, "https://external.example/art-01/repositories/v1/repo/") {
		t.Fatalf("resolvePayload = %q, %v", resolved, err)
	}
}

func TestResolveArtifactRepositoryErrors(t *testing.T) {
	addresses := map[string]string{"default": "http://artifact-manager:8081"}
	for _, tc := range []struct {
		name      string
		reference string
		want      string
	}{
		{name: "missing instance", reference: "artifact://art-01/repositories/v1/repo/", want: "has no Runner address"},
		{name: "invalid path", reference: "artifact:///repositories/../secret/", want: "invalid artifact repository URL"},
		{name: "query", reference: "artifact:///repositories/v1/repo/?token=x", want: "invalid artifact repository URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveArtifactRepository(tc.reference, addresses)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolveArtifactRepository(%q) error = %v", tc.reference, err)
			}
		})
	}
}

func TestResolveGitClonePayload(t *testing.T) {
	for _, mode := range []string{"internal", "external"} {
		t.Run(mode, func(t *testing.T) {
			routes := NewRoutes(mode, &staticTokens{token: "token"}, http.DefaultClient)
			endpoints := gateway.ServiceEndpoints{
				ArtifactManagers: map[string]gateway.ServiceEndpoint{"default": {
					RunnerInternalURL: "http://artifact:8081", RunnerExternalURL: "https://artifact.example",
				}},
				GitServer: gateway.ServiceEndpoint{
					RunnerInternalURL: "http://git:8080/proxy%20path/", RunnerExternalURL: "https://git.example/proxy%20path/",
				},
			}
			if err := routes.Update(endpoints); err != nil {
				t.Fatal(err)
			}
			job := gateway.JobResource{
				Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job"},
				Spec:     gateway.JobSpec{Payload: `{"clone_url":"git-server:///git/host/team/repo%252B.git","origin_url":"https://origin/repo.git","commit_id":"abc"}`},
			}
			if err := routes.Pin(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			endpoints.GitServer = gateway.ServiceEndpoint{
				RunnerInternalURL: "http://new-git:8080", RunnerExternalURL: "https://new-git.example",
			}
			if err := routes.Update(endpoints); err != nil {
				t.Fatal(err)
			}
			resolved, err := routes.ResolvePayload(job)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]string
			if err := json.Unmarshal([]byte(resolved), &payload); err != nil {
				t.Fatal(err)
			}
			base := "http://git:8080"
			if mode == "external" {
				base = "https://git.example"
			}
			if payload["clone_url"] != base+"/proxy%20path/git/host/team/repo%252B.git" ||
				payload["origin_url"] != "https://origin/repo.git" || payload["commit_id"] != "abc" {
				t.Fatalf("payload = %v", payload)
			}
			routes.Release(job)
			resolved, err = routes.ResolvePayload(job)
			if err != nil || !strings.Contains(resolved, "new-git") {
				t.Fatalf("refreshed payload = %q, %v", resolved, err)
			}
		})
	}
}

func TestResolveGitCloneErrors(t *testing.T) {
	for _, reference := range []string{
		"git-server://host/git/repo.git", "git-server:///git/../secret", "git-server:///git/repo.git?token=x",
	} {
		if _, err := resolveGitClone(reference, "http://git:8080"); err == nil {
			t.Fatalf("accepted invalid clone URL %q", reference)
		}
	}
	routes := NewRoutes("external", nil, http.DefaultClient)
	if err := routes.Update(gateway.ServiceEndpoints{
		ArtifactManagers: map[string]gateway.ServiceEndpoint{"default": {RunnerInternalURL: "http://artifact:8081"}},
		GitServer:        gateway.ServiceEndpoint{RunnerInternalURL: "http://git:8080"},
	}); err != nil {
		t.Fatal(err)
	}
	job := gateway.JobResource{Spec: gateway.JobSpec{Payload: `{"clone_url":"git-server:///git/host/repo.git"}`}}
	if _, err := routes.ResolvePayload(job); err == nil || !strings.Contains(err.Error(), "selected access mode") {
		t.Fatalf("missing external address error = %v", err)
	}
	job.Spec.Payload = `{"clone_url":"https://origin/repo.git","origin_url":"https://origin/repo.git"}`
	if got, err := routes.ResolvePayload(job); err != nil || got != job.Spec.Payload {
		t.Fatalf("direct clone payload = %q, %v", got, err)
	}
}
