package artifact

import (
	"context"
	"net/http"
	"testing"

	"ebs-runner/pkg/gateway"
)

func TestArtifactRoutesPinJobAnnotation(t *testing.T) {
	routes := NewRoutes("internal", &staticTokens{token: "token"}, http.DefaultClient)
	if err := routes.Update(gateway.ArtifactEndpoints{Instances: map[string]gateway.ArtifactEndpoint{
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
	if err := routes.Update(gateway.ArtifactEndpoints{Instances: map[string]gateway.ArtifactEndpoint{
		"default": {RunnerInternalURL: "http://default:8081"},
		"art-01":  {RunnerInternalURL: "http://art-01-new:8081"},
	}}); err != nil {
		t.Fatal(err)
	}
	oldClient, err := routes.client("project", "job-1")
	if err != nil || oldClient.baseURL.Host != "art-01-old:8081" {
		t.Fatalf("running Job address = %v, %v", oldClient, err)
	}
	newJob := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job-2", Annotations: job.Metadata.Annotations}}
	if err := routes.Pin(context.Background(), newJob); err != nil {
		t.Fatal(err)
	}
	newClient, err := routes.client("project", "job-2")
	if err != nil || newClient.baseURL.Host != "art-01-new:8081" {
		t.Fatalf("new Job address = %v, %v", newClient, err)
	}
	routes.Release(job)
	routes.Release(newJob)
}
