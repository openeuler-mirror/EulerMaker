package v1

import (
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestArtifactManagerContentRoundTripAndDeepCopy(t *testing.T) {
	original := &ArtifactManagerContent{Instances: map[string]ArtifactManagerInstance{
		"art-01": {
			InternalURL:       "http://artifact-manager.art-01:8081",
			RunnerInternalURL: "http://runner-endpoint:8081",
			RunnerExternalURL: "https://artifacts.example.com",
		},
	}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ArtifactManagerContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, &decoded) {
		t.Fatalf("round trip changed content: %#v", decoded)
	}
	copy := original.DeepCopy()
	copy.Instances["art-01"] = ArtifactManagerInstance{InternalURL: "http://changed:8081"}
	if !reflect.DeepEqual(original, &decoded) {
		t.Fatal("copy changed original instance map")
	}
}

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

func TestBuildGraphRoundTripAndDeepCopy(t *testing.T) {
	graph := &BuildGraph{Status: BuildGraphStatus{Nodes: map[string]DcgNodeState{
		"a": {OutDep: []string{"b"}}, "b": {InDep: map[string]VersionConst{"a": {GE: "1"}}},
	}}}
	data, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	var decoded BuildGraph
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(graph, &decoded) {
		t.Fatal("graph round trip changed nodes")
	}
	copy := graph.DeepCopy()
	copy.Status.Nodes["a"].OutDep[0] = "changed"
	copy.Status.Nodes["b"].InDep["a"] = VersionConst{GE: "2"}
	if !reflect.DeepEqual(graph, &decoded) {
		t.Fatal("DeepCopy changed original graph")
	}
}

func TestScriptDeepCopy(t *testing.T) {
	original := &ScriptList{Items: []Script{{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"owner": "ops"}}, Spec: ScriptSpec{Content: "#!/bin/sh\n"}}}}
	copied := original.DeepCopy()
	copied.Items[0].Labels["owner"] = "other"
	copied.Items[0].Spec.Content = "changed"
	if original.Items[0].Labels["owner"] != "ops" || original.Items[0].Spec.Content != "#!/bin/sh\n" {
		t.Fatal("copy mutated original")
	}
}

func TestSpecStatusGroupJSON(t *testing.T) {
	group := NewSpecStatusGroup(map[string]SpecStatus{
		"gcc":  {Build: SpecBuildStatus{Status: "Succeeded"}, Install: SpecInstallStatus{Status: "Failed"}, DispatchCount: 2},
		"bash": {},
	})
	data, err := json.Marshal(group)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"build", "install", "dispatchCount"} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("missing grouped field %q in %s", field, data)
		}
	}
	if group.Len() != 2 || group.Entry("gcc").DispatchCount != 2 || group.Entry("bash").Build.Status != "" {
		t.Fatalf("unexpected group: %+v", group)
	}
	var decoded SpecStatusGroup
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Len() != 2 || decoded.Entry("gcc").Install.Status != "Failed" {
		t.Fatalf("unexpected decoded group: %+v", decoded)
	}
}

func TestSpecStatusGroupReadsLegacyStoredLayout(t *testing.T) {
	data := []byte(`{"gcc":{"build":{"status":"Succeeded"},"install":{"status":"Failed"},"dispatchCount":2},"bash":{"build":{"status":"Running"}}}`)
	var group SpecStatusGroup
	if err := json.Unmarshal(data, &group); err != nil {
		t.Fatal(err)
	}
	if group.Len() != 2 || group.Entry("gcc").Install.Status != "Failed" || group.Entry("gcc").DispatchCount != 2 || group.Entry("bash").Build.Status != "Running" {
		t.Fatalf("legacy status not converted: %+v", group)
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
