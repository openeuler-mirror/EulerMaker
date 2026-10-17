package executor

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ebs-runner/pkg/artifact"
	"ebs-runner/pkg/gateway"
)

type staticTokens struct{ token string }

func (s *staticTokens) Token(context.Context) (string, error) { return s.token, nil }

func (s *staticTokens) RefreshAfterUnauthorized(context.Context, string) (string, error) {
	return s.token, nil
}

func TestCTExecutorWritesResolvedRepository(t *testing.T) {
	dir := t.TempDir()
	routes := artifact.NewRoutes("internal", &staticTokens{token: "token"}, http.DefaultClient)
	if err := routes.Update(gateway.ArtifactEndpoints{Instances: map[string]gateway.ArtifactEndpoint{
		"default": {RunnerInternalURL: "http://artifact-manager:8081"},
	}}); err != nil {
		t.Fatal(err)
	}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project"}, Spec: gateway.JobSpec{
		RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"}),
		Payload:     `{"repo":[{"url":"artifact:///repositories/v1/repo/","priority":10}]}`,
	}}
	executor := &CTExecutor{WorkDir: filepath.Join(dir, "work"), ResultRoot: filepath.Join(dir, "results"),
		Runtime: &fakeContainerRuntime{exitCode: 0}, ResolvePayload: routes.ResolvePayload}
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
