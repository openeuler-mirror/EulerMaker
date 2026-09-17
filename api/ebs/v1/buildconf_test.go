package v1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"testing"
)

func TestBuildConfDeepCopyAndScheme(t *testing.T) {
	original := &BuildConf{Spec: BuildConfSpec{Targets: map[string]BuildConfTarget{"os": {Arches: map[string]BuildConfArch{"arch": {Image: "original:v1"}}}}}}
	copied := original.DeepCopy()
	copied.Spec.Targets["os"].Arches["arch"] = BuildConfArch{Image: "changed:v1"}
	if original.Spec.Targets["os"].Arches["arch"].Image != "original:v1" {
		t.Fatal("DeepCopy shares maps")
	}
	list := &BuildConfList{Items: []BuildConf{*original}}
	listCopy := list.DeepCopy()
	delete(listCopy.Items[0].Spec.Targets, "os")
	if len(list.Items[0].Spec.Targets) != 1 {
		t.Fatal("list copy shares targets")
	}
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"BuildConf", "BuildConfList"} {
		if _, err := scheme.New(SchemeGroupVersion.WithKind(kind)); err != nil {
			t.Fatal(err)
		}
	}
}
