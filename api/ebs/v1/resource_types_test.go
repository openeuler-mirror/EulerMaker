package v1

import (
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestBuildInfoBootstrapRepo(t *testing.T) {
	info := &BuildInfo{Spec: BuildInfoSpec{BuildPayload: "macros:\n  dist: .oe2403\n", BootstrapRepo: []BootstrapRepo{{Name: "base", Repo: "https://example.com/repo"}}}}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var decoded BuildInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Spec, info.Spec) {
		t.Fatalf("round trip changed spec: %#v", decoded.Spec)
	}
	copy := info.DeepCopy()
	copy.Spec.BootstrapRepo[0].Repo = "changed"
	if info.Spec.BootstrapRepo[0].Repo != "https://example.com/repo" {
		t.Fatal("DeepCopy shares bootstrap repos")
	}
}

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
}

func TestSnapshotDefaultRefRoundTripAndDeepCopy(t *testing.T) {
	for _, ref := range []GitRef{{}, {Type: GitRefBranch, Value: "main"}, {Type: GitRefTag, Value: "v1"}} {
		snapshot := &Snapshot{Spec: SnapshotSpec{DefaultRef: ref}}
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Snapshot
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Spec.DefaultRef != ref {
			t.Fatalf("defaultRef=%+v, want %+v", decoded.Spec.DefaultRef, ref)
		}
		copy := snapshot.DeepCopy()
		if copy.Spec.DefaultRef != ref {
			t.Fatal("DeepCopy lost defaultRef")
		}
		copy.Spec.DefaultRef.Value = "changed"
		if snapshot.Spec.DefaultRef != ref {
			t.Fatal("copy modified original")
		}
	}
}
