package server

import (
	"io"
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
	"github.com/emicklei/go-restful/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildResourceConfigRoutesAreClusterScopedOnly(t *testing.T) {
	container := restful.NewContainer()
	container.Add(new(restful.WebService).Path("/apis/ebs/v1"))
	if err := installBuildResourceConfigRoutes(container, nil); err != nil {
		t.Fatalf("install routes: %v", err)
	}
	var globalRoutes int
	for _, route := range container.RegisteredWebServices()[0].Routes() {
		if strings.Contains(route.Path, "/projects/{project}/buildresourceconfigs") {
			t.Fatalf("project-scoped BuildResourceConfig route was registered: %s", route.Path)
		}
		if route.Path == "/apis/ebs/v1/buildresourceconfigs" || strings.HasPrefix(route.Path, "/apis/ebs/v1/buildresourceconfigs/") {
			globalRoutes++
		}
	}
	if globalRoutes != 5 {
		t.Fatalf("global routes = %d, want 5", globalRoutes)
	}
}

func TestDecodeBuildResourceConfigRejectsUnknownFields(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`{"metadata":{"name":"project-a"},"spec":{"os":"openEuler"}}`))
	if _, err := decodeBuildResourceConfig(body); err == nil {
		t.Fatal("expected unknown spec.os to be rejected")
	}
}

func TestNormalizeBuildResourceConfigIdentity(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`{"metadata":{"name":"custom-table"},"spec":{"packages":{}}}`))
	obj, err := decodeBuildResourceConfig(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := normalizeBuildResourceConfigIdentity(obj, "custom-table"); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if obj.Name != "custom-table" || obj.Namespace != "" || obj.Kind != "BuildResourceConfig" || obj.APIVersion != "ebs/v1" {
		t.Fatalf("unexpected object identity: %#v %#v", obj.TypeMeta, obj.ObjectMeta)
	}
}

func TestNormalizeBuildResourceConfigRejectsNamespace(t *testing.T) {
	obj := &ebsv1.BuildResourceConfig{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "project-a"}}
	if err := normalizeBuildResourceConfigIdentity(obj, "default"); err == nil {
		t.Fatal("expected namespace to be rejected")
	}
}
