package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestScriptDeepCopyAndScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Script", "ScriptList"} {
		if _, err := scheme.New(SchemeGroupVersion.WithKind(kind)); err != nil {
			t.Fatal(err)
		}
	}
	original := &ScriptList{Items: []Script{{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"owner": "ops"}}, Spec: ScriptSpec{Content: "#!/bin/sh\n"}}}}
	copied := original.DeepCopy()
	copied.Items[0].Labels["owner"] = "other"
	copied.Items[0].Spec.Content = "changed"
	if original.Items[0].Labels["owner"] != "ops" || original.Items[0].Spec.Content != "#!/bin/sh\n" {
		t.Fatal("copy mutated original")
	}
	var script *Script
	var list *ScriptList
	if script.DeepCopyObject() != nil || list.DeepCopyObject() != nil {
		t.Fatal("nil deepcopy must remain nil")
	}
}
