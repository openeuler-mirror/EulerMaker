package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveArtifactRepositories(t *testing.T) {
	routes := newArtifactRoutes("internal", &staticTokens{token: "token"}, http.DefaultClient)
	if err := routes.update(ArtifactEndpoints{Instances: map[string]ArtifactEndpoint{
		"default": {RunnerInternalURL: "http://default:8081"},
		"art-01":  {RunnerInternalURL: "http://art-01-old:8081"},
		"art-02":  {RunnerInternalURL: "http://art-02:8081"},
	}}); err != nil {
		t.Fatal(err)
	}
	job := JobResource{Metadata: ObjectMeta{Namespace: "project", Name: "job", Annotations: map[string]string{
		"ebs.io/artifacts": "art-01",
	}}, Spec: JobSpec{Payload: `{"repo":[{"url":"artifact://art-01/repositories/v1/one/","priority":10},{"url":"artifact://art-02/repositories/v1/two/","priority":20},{"url":"artifact:///repositories/v1/three/","priority":30},{"url":"https://bootstrap.example/repo/","priority":99}],"commit_id":"abc"}`}}
	if err := routes.pin(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := routes.update(ArtifactEndpoints{Instances: map[string]ArtifactEndpoint{
		"default": {RunnerInternalURL: "http://default:8081"},
		"art-01":  {RunnerInternalURL: "http://art-01-new:8081"},
		"art-02":  {RunnerInternalURL: "http://art-02:8081"},
	}}); err != nil {
		t.Fatal(err)
	}
	resolved, err := routes.resolvePayload(job)
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
	routes := newArtifactRoutes("external", &staticTokens{token: "token"}, http.DefaultClient)
	if err := routes.update(ArtifactEndpoints{Instances: map[string]ArtifactEndpoint{
		"art-01": {RunnerInternalURL: "http://internal:8081", RunnerExternalURL: "https://external.example/art-01"},
	}}); err != nil {
		t.Fatal(err)
	}
	job := JobResource{Spec: JobSpec{Payload: `{"repo":[{"url":"artifact://art-01/repositories/v1/repo/","priority":10}]}`}}
	resolved, err := routes.resolvePayload(job)
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

func TestCTExecutorWritesResolvedRepository(t *testing.T) {
	dir := t.TempDir()
	routes := newArtifactRoutes("internal", &staticTokens{token: "token"}, http.DefaultClient)
	if err := routes.update(ArtifactEndpoints{Instances: map[string]ArtifactEndpoint{
		"default": {RunnerInternalURL: "http://artifact-manager:8081"},
	}}); err != nil {
		t.Fatal(err)
	}
	job := JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project"}, Spec: JobSpec{
		RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"}),
		Payload:     `{"repo":[{"url":"artifact:///repositories/v1/repo/","priority":10}]}`,
	}}
	executor := &CTExecutor{WorkDir: filepath.Join(dir, "work"), ResultRoot: filepath.Join(dir, "results"),
		Runtime: &fakeContainerRuntime{exitCode: 0}, ResolvePayload: routes.resolvePayload}
	if _, err := executor.Execute(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "work", "project", "job", "payload.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "http://artifact-manager:8081/repositories/v1/repo/") {
		t.Fatalf("payload.json = %s", data)
	}
}
