package buildinfo

import (
	"reflect"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

// edge{up, down} means down depends on up: InDep[down][up] and
// outDep[up] += down.
type edge struct{ up, down string }

func graphWith(edges ...edge) map[string]*DcgNode {
	nodes := map[string]*DcgNode{}
	touch := func(name string) *DcgNode {
		if nodes[name] == nil {
			nodes[name] = &DcgNode{}
		}
		return nodes[name]
	}
	for _, e := range edges {
		up, down := touch(e.up), touch(e.down)
		if down.InDep == nil {
			down.InDep = map[string]ebsv1.VersionConst{}
		}
		down.InDep[e.up] = ebsv1.VersionConst{}
		up.OutDep = append(up.OutDep, e.down)
	}
	return nodes
}

func requireBreaks(t *testing.T, d *DcgDict, want []string) {
	t.Helper()
	if got := d.GetBootstrapBreaks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("GetBootstrapBreaks() = %v, want %v", got, want)
	}
}

func requireCycle(t *testing.T, d *DcgDict, name string, want bool) {
	t.Helper()
	if got := d.IsCycleNode(name); got != want {
		t.Fatalf("IsCycleNode(%s) = %v, want %v", name, got, want)
	}
}

func TestDAGNoCycles(t *testing.T) {
	// Diamond: b,c depend on a; d depends on b,c.
	d := NewDcgDict(graphWith(
		edge{"a", "b"}, edge{"a", "c"}, edge{"b", "d"}, edge{"c", "d"},
	))
	requireBreaks(t, d, nil)
	for _, name := range []string{"a", "b", "c", "d"} {
		requireCycle(t, d, name, false)
	}
	req := d.DispatchRequirements()
	if len(req) != 4 {
		t.Fatalf("DispatchRequirements len = %d, want 4", len(req))
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		if req[name] != 1 {
			t.Fatalf("DispatchRequirements[%s] = %d, want 1", name, req[name])
		}
	}
	if got, want := d.SortedNodes(), []string{"a", "b", "c", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SortedNodes() = %v, want %v", got, want)
	}
}

func TestSelfLoop(t *testing.T) {
	d := NewDcgDict(graphWith(edge{"a", "a"}))
	requireCycle(t, d, "a", true)
	requireBreaks(t, d, []string{"a"})
	if got := d.DispatchRequirements()["a"]; got != 2 {
		t.Fatalf("DispatchRequirements[a] = %d, want 2", got)
	}
}

func TestTwoCycleOutDepTieBreak(t *testing.T) {
	// a<->b, plus c depends on b: b has the larger outDep and wins the pick.
	d := NewDcgDict(graphWith(
		edge{"a", "b"}, edge{"b", "a"}, edge{"b", "c"},
	))
	requireCycle(t, d, "a", true)
	requireCycle(t, d, "b", true)
	requireCycle(t, d, "c", false)
	requireBreaks(t, d, []string{"b"})
	req := d.DispatchRequirements()
	if req["a"] != 2 || req["b"] != 2 || req["c"] != 1 {
		t.Fatalf("DispatchRequirements = %v, want a=2 b=2 c=1", req)
	}
}

func TestTwoCycleNameTieBreak(t *testing.T) {
	// Equal outDep: the dictionary-largest spec name wins.
	d := NewDcgDict(graphWith(edge{"a", "b"}, edge{"b", "a"}))
	requireBreaks(t, d, []string{"b"})
}

func TestCrossedCyclesSingleBreak(t *testing.T) {
	// Two cycles sharing node a: a->b->c->a and a->d->c->a. Picking a (largest
	// outDep) kills both cycles at once (design 7.2.1 rule 3).
	d := NewDcgDict(graphWith(
		edge{"a", "b"}, edge{"b", "c"}, edge{"c", "a"},
		edge{"a", "d"}, edge{"d", "c"},
	))
	for _, name := range []string{"a", "b", "c", "d"} {
		requireCycle(t, d, name, true)
	}
	requireBreaks(t, d, []string{"a"})
}

func TestMultipleSCCsDeterministic(t *testing.T) {
	build := func() *DcgDict {
		return NewDcgDict(graphWith(
			edge{"x", "y"}, edge{"y", "x"},
			edge{"a", "b"}, edge{"b", "a"},
		))
	}
	// Each cycle picks its dictionary-largest member; the result is sorted
	// and stable regardless of SCC processing order / map iteration.
	want := []string{"b", "y"}
	for i := 0; i < 20; i++ {
		requireBreaks(t, build(), want)
	}
}

func TestSortedNodesWithCycle(t *testing.T) {
	// a<->b cycle, c depends on a: the traversal covers every node exactly
	// once and is deterministic.
	d := NewDcgDict(graphWith(
		edge{"a", "b"}, edge{"b", "a"}, edge{"a", "c"},
	))
	first := d.SortedNodes()
	if len(first) != 3 {
		t.Fatalf("SortedNodes len = %d, want 3", len(first))
	}
	seen := map[string]bool{}
	for _, name := range first {
		if seen[name] {
			t.Fatalf("SortedNodes duplicate %s: %v", name, first)
		}
		seen[name] = true
	}
	for i := 0; i < 20; i++ {
		if got := d.SortedNodes(); !reflect.DeepEqual(got, first) {
			t.Fatalf("SortedNodes not deterministic: %v vs %v", got, first)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	nodes := graphWith(edge{"a", "b"}, edge{"b", "a"}, edge{"b", "c"})
	nodes["a"].Version = "1.0-1"
	d := NewDcgDict(nodes)

	state := d.ToState()
	if state["c"].Version != "NA" {
		t.Fatalf("empty Version persisted as %q, want NA", state["c"].Version)
	}
	loaded := DcgDictFromState(state)
	requireBreaks(t, loaded, d.GetBootstrapBreaks())
	for _, name := range []string{"a", "b", "c"} {
		if loaded.IsCycleNode(name) != d.IsCycleNode(name) {
			t.Fatalf("cycle mismatch for %s after round trip", name)
		}
		got, want := loaded.Node(name), d.Node(name)
		if !reflect.DeepEqual(got.OutDep, want.OutDep) ||
			!reflect.DeepEqual(got.InDep, want.InDep) ||
			!reflect.DeepEqual(got.InstallInDep, want.InstallInDep) ||
			got.BootstrapBreak != want.BootstrapBreak {
			t.Fatalf("node %s mismatch after round trip: %+v vs %+v", name, got, want)
		}
	}
	if loaded.Node("a").Version != "1.0-1" {
		t.Fatalf("Version lost in round trip: %q", loaded.Node("a").Version)
	}
}

func TestLoadDoesNotReselectBreaks(t *testing.T) {
	// G-09: a persisted cycle without break marks stays without breaks on
	// load; cycleNodes is still recomputed from the edge set.
	state := map[string]ebsv1.DcgNodeState{
		"a": {OutDep: []string{"b"}, InDep: map[string]ebsv1.VersionConst{"b": {}}},
		"b": {OutDep: []string{"a"}, InDep: map[string]ebsv1.VersionConst{"a": {}}},
	}
	loaded := DcgDictFromState(state)
	requireBreaks(t, loaded, nil)
	requireCycle(t, loaded, "a", true)
	requireCycle(t, loaded, "b", true)
	if got := loaded.DispatchRequirements()["a"]; got != 2 {
		t.Fatalf("DispatchRequirements[a] = %d, want 2", got)
	}
}

func TestAddInstallEdge(t *testing.T) {
	d := NewDcgDict(graphWith(edge{"a", "b"}, edge{"z", "y"}))
	vc := ebsv1.VersionConst{GE: "1.0"}

	if d.AddInstallEdge("missing", "a", vc) || d.AddInstallEdge("a", "missing", vc) {
		t.Fatal("AddInstallEdge with missing endpoint must return false")
	}
	if !d.AddInstallEdge("a", "z", vc) {
		t.Fatal("AddInstallEdge must return true for a new edge")
	}
	if got := d.Node("a").InstallInDep["z"]; !reflect.DeepEqual(got, vc) {
		t.Fatalf("installInDep[a][z] = %+v, want %+v", got, vc)
	}
	if !reflect.DeepEqual(d.Node("z").OutDep, []string{"a", "y"}) {
		t.Fatalf("outDep[z] = %v, want [a y] (sorted)", d.Node("z").OutDep)
	}
	if d.AddInstallEdge("a", "z", vc) {
		t.Fatal("AddInstallEdge must be idempotent")
	}
	if got := len(d.Node("z").OutDep); got != 2 {
		t.Fatalf("outDep[z] grew on duplicate edge: %v", d.Node("z").OutDep)
	}
}

func TestRefreshCyclesAndBreaksAppendsNewCycle(t *testing.T) {
	// Chains a->b and c->d: no cycles initially.
	d := NewDcgDict(graphWith(edge{"a", "b"}, edge{"c", "d"}))
	requireBreaks(t, d, nil)

	candidate := d.Clone()
	if !candidate.AddInstallEdge("a", "b", ebsv1.VersionConst{}) {
		t.Fatal("candidate edge append failed")
	}
	picks := candidate.RefreshCyclesAndBreaks()
	if !reflect.DeepEqual(picks, []string{"b"}) {
		t.Fatalf("RefreshCyclesAndBreaks picks = %v, want [b]", picks)
	}
	requireCycle(t, candidate, "a", true)
	requireCycle(t, candidate, "b", true)
	requireCycle(t, candidate, "c", false)
	requireBreaks(t, candidate, []string{"b"})

	// The live graph is untouched (candidate isolation, 7.4.7).
	requireBreaks(t, d, nil)
	requireCycle(t, d, "a", false)

	// Refresh is idempotent: the existing break already peels the cycle.
	if again := candidate.RefreshCyclesAndBreaks(); len(again) != 0 {
		t.Fatalf("second refresh picked %v, want none", again)
	}
}

func TestRefreshPreservesInitialBreaks(t *testing.T) {
	// Initial cycle a<->b picks b (tie -> dictionary max). A runtime install
	// edge pair a<->c merges c into the SCC and adds a new cycle a<->c that
	// does not pass b: a new break is appended; b is never re-selected
	// (G-09). a and c tie on full outDep length (a: [b c], c: [a e]), so the
	// dictionary-largest c wins.
	d := NewDcgDict(graphWith(
		edge{"a", "b"}, edge{"b", "a"},
		edge{"c", "e"},
	))
	requireBreaks(t, d, []string{"b"})

	candidate := d.Clone()
	candidate.AddInstallEdge("c", "a", ebsv1.VersionConst{})
	candidate.AddInstallEdge("a", "c", ebsv1.VersionConst{})
	picks := candidate.RefreshCyclesAndBreaks()
	if !reflect.DeepEqual(picks, []string{"c"}) {
		t.Fatalf("new cycle picks = %v, want [c]", picks)
	}
	requireBreaks(t, candidate, []string{"b", "c"})
	for _, name := range []string{"a", "b", "c"} {
		requireCycle(t, candidate, name, true)
	}
	requireCycle(t, candidate, "e", false)
}

func TestRefreshSkipsCycleAlreadyBroken(t *testing.T) {
	// A new edge landing inside an already-broken cycle adds no break: the
	// pre-removed break keeps the merged SCC peelable.
	d := NewDcgDict(graphWith(
		edge{"a", "b"}, edge{"b", "a"}, edge{"b", "c"},
	))
	requireBreaks(t, d, []string{"b"})

	candidate := d.Clone()
	// c install-depends on a: edge a->c. Cycle b->c ... c has no path back,
	// so no new cycle appears.
	candidate.AddInstallEdge("c", "a", ebsv1.VersionConst{})
	if picks := candidate.RefreshCyclesAndBreaks(); len(picks) != 0 {
		t.Fatalf("no new cycle: picks = %v, want none", picks)
	}
	requireBreaks(t, candidate, []string{"b"})
}

func TestNodeAccessors(t *testing.T) {
	d := NewDcgDict(graphWith(edge{"a", "b"}))
	if d.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", d.Len())
	}
	if got := d.Names(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Names() = %v", got)
	}
	if d.Node("missing") != nil {
		t.Fatal("Node(missing) must be nil")
	}
	if got := d.Node("b").InDegree(); got != 1 {
		t.Fatalf("InDegree(b) = %d, want 1", got)
	}
}
