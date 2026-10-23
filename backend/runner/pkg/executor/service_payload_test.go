package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ebs-runner/pkg/gateway"
	"ebs-runner/pkg/service"
)

type staticTokens struct{ token string }

func (s *staticTokens) Token(context.Context) (string, error) { return s.token, nil }

func (s *staticTokens) RefreshAfterUnauthorized(context.Context, string) (string, error) {
	return s.token, nil
}

func TestCTExecutorWritesResolvedServiceURLs(t *testing.T) {
	for _, mode := range []string{"internal", "external"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			routes := service.NewRoutes(mode, &staticTokens{token: "token"}, http.DefaultClient)
			if err := routes.Update(gateway.ServiceEndpoints{ArtifactManagers: map[string]gateway.ServiceEndpoint{
				"default": {RunnerInternalURL: "http://artifact-manager:8081", RunnerExternalURL: "https://artifacts.example/art-01"},
			}, GitServer: gateway.ServiceEndpoint{RunnerInternalURL: "http://git-server:8080/proxy"}}); err != nil {
				t.Fatal(err)
			}
			job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project"}, Spec: gateway.JobSpec{
				RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"}),
				Payload:     `{"origin_url":"https://atomgit.com/src-openeuler/example.git","clone_url":"git-server:///git/atomgit.com/src-openeuler/example.git","commit_id":"abc","repo":[{"url":"artifact:///repositories/v1/repo/","priority":10}]}`,
			}}
			original := job.Spec.Payload
			executor := &CTExecutor{WorkDir: filepath.Join(dir, "work"), ResultRoot: filepath.Join(dir, "results"),
				Runtime: &fakeContainerRuntime{exitCode: 0}, ResolvePayload: routes.ResolvePayload}
			if _, err := executor.Execute(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "work", "project", "job", "payload.json"))
			if err != nil {
				t.Fatal(err)
			}
			wantRepo := "http://artifact-manager:8081/repositories/v1/repo/"
			wantClone := "http://git-server:8080/proxy/git/atomgit.com/src-openeuler/example.git"
			if mode == "external" {
				wantRepo = "https://artifacts.example/art-01/repositories/v1/repo/"
				wantClone = "https://atomgit.com/src-openeuler/example.git"
			}
			if !strings.Contains(string(data), wantRepo) {
				t.Fatalf("payload.json = %s", data)
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatal(err)
			}
			if got := payload["origin_url"]; got != "https://atomgit.com/src-openeuler/example.git" {
				t.Fatalf("origin_url = %v", got)
			}
			if payload["clone_url"] != wantClone || payload["commit_id"] != "abc" || job.Spec.Payload != original {
				t.Fatalf("source fields = %v", payload)
			}
		})
	}
}
