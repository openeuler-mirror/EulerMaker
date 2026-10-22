package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"ebs-runner/pkg/gateway"
)

type staticTokens struct{ token string }

func (s *staticTokens) Token(context.Context) (string, error) { return s.token, nil }

func (s *staticTokens) RefreshAfterUnauthorized(context.Context, string) (string, error) {
	return s.token, nil
}

func TestArtifactRoutesSelectExternalAddress(t *testing.T) {
	routes := NewRoutes("external", &staticTokens{token: "token"}, http.DefaultClient)
	if err := routes.Update(gateway.ServiceEndpoints{ArtifactManagers: map[string]gateway.ServiceEndpoint{
		"default": {RunnerInternalURL: "http://internal:8081", RunnerExternalURL: "https://external.example"},
		"art-01":  {RunnerInternalURL: "http://internal:8081"},
	}}); err != nil {
		t.Fatal(err)
	}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job-external"}}
	if err := routes.Pin(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	client, err := routes.client("project", "job-external")
	if err != nil || client == nil || routes.jobs["project/job-external"].artifactURL != "https://external.example" {
		t.Fatalf("client=%v error=%v", client, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job.Metadata.Name = "job-waiting"
	job.Metadata.Annotations = map[string]string{"ebs.io/artifacts": "art-01"}
	if err := routes.Pin(ctx, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("missing external address: %v", err)
	}
}

func TestArtifactRoutesPinJobAnnotation(t *testing.T) {
	routes := NewRoutes("internal", &staticTokens{token: "token"}, http.DefaultClient)
	if err := routes.Update(gateway.ServiceEndpoints{ArtifactManagers: map[string]gateway.ServiceEndpoint{
		"default": {RunnerInternalURL: "http://default:8081"},
		"art-01":  {RunnerInternalURL: "http://art-01-old:8081"},
	}}); err != nil {
		t.Fatal(err)
	}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job-1", Annotations: map[string]string{
		"ebs.io/artifacts": "art-01",
	}}}
	if err := routes.Pin(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := routes.Update(gateway.ServiceEndpoints{ArtifactManagers: map[string]gateway.ServiceEndpoint{
		"default": {RunnerInternalURL: "http://default:8081"},
		"art-01":  {RunnerInternalURL: "http://art-01-new:8081"},
	}}); err != nil {
		t.Fatal(err)
	}
	oldClient, err := routes.client("project", "job-1")
	if err != nil || oldClient == nil || routes.jobs["project/job-1"].artifactURL != "http://art-01-old:8081" {
		t.Fatalf("running Job address = %v, %v", oldClient, err)
	}
	newJob := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job-2", Annotations: job.Metadata.Annotations}}
	if err := routes.Pin(context.Background(), newJob); err != nil {
		t.Fatal(err)
	}
	newClient, err := routes.client("project", "job-2")
	if err != nil || newClient == nil || routes.jobs["project/job-2"].artifactURL != "http://art-01-new:8081" {
		t.Fatalf("new Job address = %v, %v", newClient, err)
	}
	routes.Release(job)
	routes.Release(newJob)
}
