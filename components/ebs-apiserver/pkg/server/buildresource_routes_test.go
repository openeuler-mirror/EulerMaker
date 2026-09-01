package server

import (
	"io"
	"strings"
	"testing"

	"github.com/emicklei/go-restful/v3"
)

func TestBuildResourceRoutesAreProjectScopedOnly(t *testing.T) {
	container := restful.NewContainer()
	container.Add(new(restful.WebService).Path("/apis/ebs/v1"))
	if err := installBuildResourceRoutes(container, nil); err != nil {
		t.Fatalf("install routes: %v", err)
	}
	var projectRoutes int
	for _, route := range container.RegisteredWebServices()[0].Routes() {
		if route.Path == "/apis/ebs/v1/buildresources" || strings.HasPrefix(route.Path, "/apis/ebs/v1/buildresources/") {
			t.Fatalf("global BuildResource route was registered: %s", route.Path)
		}
		if strings.Contains(route.Path, "/projects/{project}/buildresources") {
			projectRoutes++
		}
	}
	if projectRoutes != 5 {
		t.Fatalf("project routes = %d, want 5", projectRoutes)
	}
}

func TestDecodeBuildResourceRejectsUnknownFields(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`{"metadata":{"name":"project-a"},"spec":{"os":"openEuler"}}`))
	if _, err := decodeBuildResource(body); err == nil {
		t.Fatal("expected unknown spec.os to be rejected")
	}
}

func TestNormalizeBuildResourceIdentity(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`{"spec":{"packages":{}}}`))
	obj, err := decodeBuildResource(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := normalizeBuildResourceIdentity(obj, "project-a", "project-a"); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if obj.Name != "project-a" || obj.Namespace != "project-a" || obj.Kind != "BuildResource" || obj.APIVersion != "ebs/v1" {
		t.Fatalf("unexpected object identity: %#v %#v", obj.TypeMeta, obj.ObjectMeta)
	}
}
