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
			original := job.Spec.Payload
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
			wantClone := "http://git:8080/proxy%20path/git/host/team/repo%252B.git"
			if mode == "external" {
				wantClone = "https://origin/repo.git"
			}
			if payload["clone_url"] != wantClone ||
				payload["origin_url"] != "https://origin/repo.git" || payload["commit_id"] != "abc" {
				t.Fatalf("payload = %v", payload)
			}
			routes.Release(job)
			resolved, err = routes.ResolvePayload(job)
			if err != nil {
				t.Fatalf("refreshed payload = %q, %v", resolved, err)
			}
			if err := json.Unmarshal([]byte(resolved), &payload); err != nil {
				t.Fatal(err)
			}
			if mode == "internal" {
				wantClone = "http://new-git:8080/git/host/team/repo%252B.git"
			}
			if payload["clone_url"] != wantClone || job.Spec.Payload != original {
				t.Fatalf("refreshed payload = %v; original Job payload = %s", payload, job.Spec.Payload)
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
	routes := NewRoutes("internal", nil, http.DefaultClient)
	if err := routes.Update(gateway.ServiceEndpoints{
		ArtifactManagers: map[string]gateway.ServiceEndpoint{"default": {RunnerInternalURL: "http://artifact:8081"}},
		GitServer:        gateway.ServiceEndpoint{RunnerExternalURL: "https://git.example"},
	}); err != nil {
		t.Fatal(err)
	}
	job := gateway.JobResource{Spec: gateway.JobSpec{Payload: `{"clone_url":"git-server:///git/host/repo.git"}`}}
	if _, err := routes.ResolvePayload(job); err == nil || !strings.Contains(err.Error(), "selected access mode") {
		t.Fatalf("missing internal address error = %v", err)
	}
	job.Spec.Payload = `{"clone_url":"https://origin/repo.git","origin_url":"https://origin/repo.git"}`
	if got, err := routes.ResolvePayload(job); err != nil || got != job.Spec.Payload {
		t.Fatalf("direct clone payload = %q, %v", got, err)
	}
}

func TestExternalCloneUsesOriginWithoutGitServer(t *testing.T) {
	routes := NewRoutes("external", nil, http.DefaultClient)
	for _, cloneURL := range []string{"git-server:///git/host/repo.git", "https://mirror.example/repo.git"} {
		input := map[string]any{
			"clone_url": cloneURL, "origin_url": "https://origin.example/repo.git",
			"commit_id": "0123456789abcdef0123456789abcdef01234567", "custom": map[string]any{"enabled": true},
		}
		data, _ := json.Marshal(input)
		job := gateway.JobResource{Spec: gateway.JobSpec{Payload: string(data)}}
		resolved, err := routes.ResolvePayload(job)
		if err != nil {
			t.Fatal(err)
		}
		var output map[string]any
		if err := json.Unmarshal([]byte(resolved), &output); err != nil {
			t.Fatal(err)
		}
		input["clone_url"] = input["origin_url"]
		want, _ := json.Marshal(input)
		got, _ := json.Marshal(output)
		if string(got) != string(want) || job.Spec.Payload != string(data) {
			t.Fatalf("local payload = %s, want %s; Job payload = %s", got, want, job.Spec.Payload)
		}
	}
}

func TestExternalCloneRequiresOrigin(t *testing.T) {
	routes := NewRoutes("external", nil, http.DefaultClient)
	for _, origin := range []string{"", `,"origin_url":""`, `,"origin_url":"  "`, `,"origin_url":null`, `,"origin_url":42`} {
		job := gateway.JobResource{Spec: gateway.JobSpec{Payload: `{"clone_url":"git-server:///git/host/repo.git"` + origin + `}`}}
		if _, err := routes.ResolvePayload(job); err == nil || !strings.Contains(err.Error(), "origin_url") {
			t.Fatalf("payload %s: error = %v", job.Spec.Payload, err)
		}
	}
	job := gateway.JobResource{Spec: gateway.JobSpec{Payload: `{"command":"custom"}`}}
	if got, err := routes.ResolvePayload(job); err != nil || got != job.Spec.Payload {
		t.Fatalf("payload without clone = %q, %v", got, err)
	}
}
