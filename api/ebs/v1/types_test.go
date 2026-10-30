package v1

import (
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildSpecPublishJSON(t *testing.T) {
	for _, publish := range []bool{false, true} {
		original := BuildSpec{BuildType: BuildTypeFull, Arch: "aarch64", Publish: &publish}
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"buildType":"full","arch":"aarch64","publish":false}`
		if publish {
			want = `{"buildType":"full","arch":"aarch64","publish":true}`
		}
		if string(data) != want {
			t.Fatalf("serialized target = %s, want %s", data, want)
		}
		var decoded BuildSpec
		if err := json.Unmarshal([]byte(want), &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, original) {
			t.Fatalf("decoded target = %+v, want %+v", decoded, original)
		}
		copy := original.DeepCopy()
		*copy.Publish = !publish
		if *original.Publish != publish {
			t.Fatal("DeepCopy shares the publish pointer")
		}
	}
}

func TestRepositoryAndRunnerJSONNames(t *testing.T) {
	var status PackageRepoStatus
	if err := json.Unmarshal([]byte(`{"cloneURL":"git-server:///git/repo.git","commitID":"abc"}`), &status); err != nil {
		t.Fatal(err)
	}
	if status.CloneURL != "git-server:///git/repo.git" || status.CommitID != "abc" {
		t.Fatalf("repository status = %+v", status)
	}
	var runner RunnerSpec
	if err := json.Unmarshal([]byte(`{"instanceID":"runner-instance"}`), &runner); err != nil {
		t.Fatal(err)
	}
	if runner.InstanceID != "runner-instance" {
		t.Fatalf("runner spec = %+v", runner)
	}
	for _, tc := range []struct {
		value  any
		fields []string
	}{
		{status, []string{"cloneURL", "commitID"}},
		{runner, []string{"instanceID"}},
	} {
		data, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range tc.fields {
			if _, ok := fields[name]; !ok {
				t.Fatalf("missing %s in %s", name, data)
			}
		}
	}
}

func TestServiceConfigContentRoundTripAndDeepCopy(t *testing.T) {
	original := &ServiceConfigContent{
		GitServer: ServiceEndpoints{
			InternalURL:       "http://git-server:8080",
			RunnerInternalURL: "http://git.internal:8080",
			RunnerExternalURL: "https://git.example.com/prefix",
		},
		ArtifactManagers: map[string]ServiceEndpoints{
			"art-01": {
				InternalURL:       "http://artifact-manager.art-01:8081",
				RunnerInternalURL: "http://runner-endpoint:8081",
				RunnerExternalURL: "https://artifacts.example.com",
			},
		},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ServiceConfigContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, &decoded) {
		t.Fatalf("round trip changed content: %#v", decoded)
	}
	copy := original.DeepCopy()
	copy.ArtifactManagers["art-01"] = ServiceEndpoints{InternalURL: "http://changed:8081"}
	copy.GitServer.InternalURL = "http://changed:8080"
	copy.GitServer.RunnerInternalURL = "http://changed:8084"
	copy.GitServer.RunnerExternalURL = "https://changed.example.com"
	if !reflect.DeepEqual(original, &decoded) {
		t.Fatal("copy changed original instance map")
	}
}

func TestBuildInfoBootstrapRepo(t *testing.T) {
	info := &BuildInfo{}
	if err := json.Unmarshal([]byte(`{"spec":{"buildPayload":"macros:\n  dist: .oe2403\n","bootstrapRepos":[{"name":"base","url":"https://example.com/repo"}]}}`), info); err != nil {
		t.Fatal(err)
	}
	if len(info.Spec.BootstrapRepos) != 1 || info.Spec.BootstrapRepos[0].URL != "https://example.com/repo" {
		t.Fatalf("unexpected bootstrap repositories: %+v", info.Spec.BootstrapRepos)
	}
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
	copy.Spec.BootstrapRepos[0].URL = "changed"
	if info.Spec.BootstrapRepos[0].URL != "https://example.com/repo" {
		t.Fatal("DeepCopy shares bootstrap repos")
	}
}

func TestBuildGraphRoundTripAndDeepCopy(t *testing.T) {
	graph := &BuildGraph{Status: BuildGraphStatus{Nodes: map[string]BuildGraphNode{
		"a": {OutDep: []string{"b"}}, "b": {InDep: map[string]VersionConstraint{"a": {GE: "1"}}},
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
	copy.Status.Nodes["b"].InDep["a"] = VersionConstraint{GE: "2"}
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

func TestSpecResultsJSONAndDeepCopy(t *testing.T) {
	results := SpecResults{
		"gcc/gcc.spec":         {SpecName: "gcc", Status: SpecInstallFailed, DispatchCount: 2, MissingDeps: map[string]MissingDep{"libc": {}}},
		"bash/bash.spec":       {Status: SpecPrepareFailed, Message: "rpmspec failed"},
		"missing/missing.spec": {SpecName: "missing", Status: SpecRpmDependsMissing},
		"unreadable/":          {Status: SpecBuildFailed, Message: "dispatch stopped; see BuildInfo conditions"},
	}
	data, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SpecResults
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(results, decoded) {
		t.Fatalf("round trip changed results: %+v", decoded)
	}
	info := &BuildInfo{Status: BuildInfoStatus{SpecResults: results}}
	copy := info.DeepCopy()
	result := copy.Status.SpecResults["gcc/gcc.spec"]
	delete(result.MissingDeps, "libc")
	copy.Status.SpecResults["gcc/gcc.spec"] = result
	if len(info.Status.SpecResults["gcc/gcc.spec"].MissingDeps) != 1 {
		t.Fatal("DeepCopy shares missing dependencies")
	}
	if got := results.FailedPackages(); !reflect.DeepEqual(got, []string{"bash", "gcc", "missing", "unreadable"}) {
		t.Fatalf("failed repositories = %v", got)
	}
	if SpecResultRepo("gcc/gcc.spec") != "gcc" {
		t.Fatal("repository extraction failed")
	}
}

func TestPackageRepoCommitIDRoundTrip(t *testing.T) {
	const commitID = "0123456789abcdef0123456789abcdef01234567"
	var repo PackageRepo
	if err := json.Unmarshal([]byte(`{"name":"gcc","ref":"refs/heads/main","commitID":"`+commitID+`"}`), &repo); err != nil {
		t.Fatal(err)
	}
	project := &Project{Spec: ProjectSpec{PackageRepos: []PackageRepo{repo}}}
	copy := project.DeepCopy()
	data, err := json.Marshal(copy.Spec.PackageRepos[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["commitID"] != commitID || repo.CommitID != commitID {
		t.Fatalf("commitID round trip: %s", data)
	}
	copy.Spec.PackageRepos[0].CommitID = ""
	if project.Spec.PackageRepos[0].CommitID != commitID {
		t.Fatal("copy mutated Project")
	}
}

func TestSnapshotDefaultRefRoundTripAndDeepCopy(t *testing.T) {
	for _, ref := range []string{"", "refs/heads/main", "refs/tags/v1"} {
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
		copy.Spec.DefaultRef = "changed"
		if snapshot.Spec.DefaultRef != ref {
			t.Fatal("copy modified original")
		}
	}
}
