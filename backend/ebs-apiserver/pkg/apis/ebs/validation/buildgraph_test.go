package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func graphFixture() *ebsv1.BuildGraph {
	info := &ebsv1.BuildInfo{ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "project", UID: "owner"}}
	return &ebsv1.BuildGraph{
		ObjectMeta: metav1.ObjectMeta{Name: info.Name, Namespace: info.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(info, ebsv1.SchemeGroupVersion.WithKind("BuildInfo"))}},
		Status: ebsv1.BuildGraphStatus{Nodes: map[string]ebsv1.DcgNodeState{
			"a": {Version: "1", OutDep: []string{"b"}},
			"b": {Version: "2", InDep: map[string]ebsv1.VersionConstraint{"a": {GE: "1"}}},
		}},
	}
}

func TestBuildGraphValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ebsv1.BuildGraph)
		valid  bool
	}{
		{"build edges", func(*ebsv1.BuildGraph) {}, true},
		{"shared build and install provider", func(g *ebsv1.BuildGraph) {
			a, b := g.Status.Nodes["a"], g.Status.Nodes["b"]
			b.InstallInDep = map[string]ebsv1.VersionConstraint{"a": {}}
			a.OutDep = []string{"b", "b"}
			g.Status.Nodes["a"], g.Status.Nodes["b"] = a, b
		}, true},
		{"empty graph", func(g *ebsv1.BuildGraph) { g.Status.Nodes = map[string]ebsv1.DcgNodeState{} }, true},
		{"cycle", func(g *ebsv1.BuildGraph) {
			a, b := g.Status.Nodes["a"], g.Status.Nodes["b"]
			a.InstallInDep = map[string]ebsv1.VersionConstraint{"b": {}}
			b.OutDep = []string{"a"}
			g.Status.Nodes["a"], g.Status.Nodes["b"] = a, b
		}, true},
		{"missing nodes", func(g *ebsv1.BuildGraph) { g.Status.Nodes = nil }, false},
		{"dangling edge", func(g *ebsv1.BuildGraph) { delete(g.Status.Nodes, "a") }, false},
		{"incorrect reverse edge", func(g *ebsv1.BuildGraph) { g.Status.Nodes["a"] = ebsv1.DcgNodeState{} }, false},
		{"missing owner", func(g *ebsv1.BuildGraph) { g.OwnerReferences = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := graphFixture()
			tc.change(graph)
			if errs := ValidateBuildGraph(graph); (len(errs) == 0) != tc.valid {
				t.Fatalf("errors: %v", errs)
			}
		})
	}
}

func TestBuildGraphStatusUpdate(t *testing.T) {
	old := graphFixture()
	updated := old.DeepCopy()
	a, b := updated.Status.Nodes["a"], updated.Status.Nodes["b"]
	a.InstallInDep = map[string]ebsv1.VersionConstraint{"b": {}}
	a.BootstrapBreak = true
	b.OutDep = []string{"a"}
	updated.Status.Nodes["a"], updated.Status.Nodes["b"] = a, b
	if errs := ValidateBuildGraphStatusUpdate(updated, old); len(errs) != 0 {
		t.Fatal(errs)
	}
	if errs := ValidateBuildGraphStatusUpdate(old, updated); len(errs) == 0 {
		t.Fatal("existing edges and breakpoints must be retained")
	}
	for _, change := range []func(*ebsv1.BuildGraph){
		func(g *ebsv1.BuildGraph) { n := g.Status.Nodes["a"]; n.Version = "new"; g.Status.Nodes["a"] = n },
		func(g *ebsv1.BuildGraph) { g.Status.Nodes["c"] = ebsv1.DcgNodeState{} },
		func(g *ebsv1.BuildGraph) { g.OwnerReferences[0].UID = "replacement" },
	} {
		g := old.DeepCopy()
		change(g)
		if errs := ValidateBuildGraphStatusUpdate(g, old); len(errs) == 0 {
			t.Fatal("immutable graph input changed")
		}
	}
}
