package buildinfo

import (
	"context"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	clientpkg "controller-manager/pkg/clients/apiserver"
	ebsv1 "ebs-api/ebs/v1"
)

func (f *fakeClient) SeedBuildGraph(info *ebsv1.BuildInfo, nodes map[string]ebsv1.DcgNodeState) *ebsv1.BuildGraph {
	f.SeedBuildInfo(info)
	f.mu.Lock()
	defer f.mu.Unlock()
	graph := newBuildGraph(info, nodes)
	graph.UID = types.UID(f.nextUIDLocked())
	graph.ResourceVersion = f.nextRVLocked()
	f.graphs[cacheKey(info.Namespace, info.Name)] = graph.DeepCopy()
	return graph.DeepCopy()
}

func (f *fakeClient) GetBuildGraph(_ context.Context, project, name string, metadataOnly bool) (*ebsv1.BuildGraph, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.graphReads = append(f.graphReads, metadataOnly)
	if err := f.consumeInjectedReadLocked("buildgraphs"); err != nil {
		return nil, err
	}
	graph := f.graphs[cacheKey(project, name)]
	if graph == nil {
		return nil, ErrNotFound
	}
	result := graph.DeepCopy()
	if metadataOnly {
		result.Status = ebsv1.BuildGraphStatus{}
	}
	return result, nil
}

func (f *fakeClient) CreateBuildGraph(_ context.Context, project string, obj *ebsv1.BuildGraph) (*ebsv1.BuildGraph, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := cacheKey(project, obj.Name)
	if f.graphs[key] != nil {
		return nil, &clientpkg.WriteError{Outcome: clientpkg.WriteRejected, StatusCode: 409,
			Err: apierrors.NewAlreadyExists(ebsv1.Resource("buildgraphs"), obj.Name)}
	}
	if failure := f.consumeInjectedWriteLocked("create-graph"); failure != nil {
		if failure.persist {
			f.persistGraphLocked(obj)
		}
		return nil, injectedFake("create-graph", "buildgraphs", failure)
	}
	return f.persistGraphLocked(obj), nil
}

func (f *fakeClient) UpdateBuildGraphStatus(_ context.Context, obj *ebsv1.BuildGraph) (*ebsv1.BuildGraph, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.graphs[cacheKey(obj.Namespace, obj.Name)]
	if old == nil {
		return nil, rejectedFake("update-graph", "buildgraphs", 404, fmt.Errorf("not found"))
	}
	if old.ResourceVersion != obj.ResourceVersion {
		return nil, rejectedFake("update-graph", "buildgraphs", 409, fmt.Errorf("conflict"))
	}
	if failure := f.consumeInjectedWriteLocked("update-graph"); failure != nil {
		if failure.persist {
			f.persistGraphLocked(obj)
		}
		return nil, injectedFake("update-graph", "buildgraphs", failure)
	}
	return f.persistGraphLocked(obj), nil
}

func (f *fakeClient) persistGraphLocked(obj *ebsv1.BuildGraph) *ebsv1.BuildGraph {
	graph := obj.DeepCopy()
	if graph.UID == "" {
		graph.UID = types.UID(f.nextUIDLocked())
	}
	graph.ResourceVersion = f.nextRVLocked()
	f.graphs[cacheKey(graph.Namespace, graph.Name)] = graph
	f.graphWrites++
	return graph.DeepCopy()
}

func testGraph(t *testing.T, client *fakeClient, info *ebsv1.BuildInfo) *ebsv1.BuildGraph {
	t.Helper()
	graph, err := client.GetBuildGraph(context.Background(), info.Namespace, info.Name, false)
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func TestBuildGraphCacheAndRecovery(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoProcessing))
	graph := client.SeedBuildGraph(bi, dcgStateAB())
	round := &reconcileRound{key: cacheKey(bi.Namespace, bi.Name), current: bi}
	dcg, stop, result, err := c.loadGraph(context.Background(), round)
	if err != nil || !stop || result.RequeueAfter != 0 || dcg == nil {
		t.Fatalf("load: %v %v", result, err)
	}
	reads := len(client.graphReads)
	got, _, _, err := c.loadGraph(context.Background(), round)
	if err != nil || got != dcg || len(client.graphReads) != reads+1 || !client.graphReads[reads] {
		t.Fatal("cache hit must only read metadata")
	}
	graph.Status.Nodes["a"] = ebsv1.DcgNodeState{Version: "changed", OutDep: []string{"b"}}
	if _, err := client.UpdateBuildGraphStatus(context.Background(), graph); err != nil {
		t.Fatal(err)
	}
	got, _, _, err = c.loadGraph(context.Background(), round)
	if err != nil || got.Node("a").Version != "changed" {
		t.Fatal("new version not loaded")
	}
	delete(client.graphs, round.key)
	got, stop, _, err = c.loadGraph(context.Background(), round)
	if err != nil || !stop || got != nil {
		t.Fatal("missing graph must stop processing")
	}
	cond := findCondition(round.current.Status.Conditions, conditionBuildGraphUnavailable)
	if cond == nil || cond.Reason != "BuildGraphMissing" {
		t.Fatalf("condition: %v", cond)
	}
	client.SeedBuildGraph(round.current, dcgStateAB())
	if got, _, _, err = c.loadGraph(context.Background(), round); err != nil || got == nil {
		t.Fatal("recovery failed", err)
	}
	if findCondition(round.current.Status.Conditions, conditionBuildGraphUnavailable) != nil {
		t.Fatal("recovery must clear condition")
	}
}

func TestBuildGraphUnknownCreate(t *testing.T) {
	for _, persist := range []bool{true, false} {
		t.Run(fmt.Sprint(persist), func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			bi := client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
			round := &reconcileRound{key: cacheKey(bi.Namespace, bi.Name), current: bi}
			client.injectedWrites["create-graph"] = &injectedWrite{outcome: clientpkg.WriteUnknown, persist: persist}
			d, result, err := c.writeGraph(context.Background(), round, DcgDictFromState(dcgStateAB()))
			if err != nil {
				t.Fatal(err)
			}
			if persist && (d == nil || round.graph == nil) {
				t.Fatal("successful confirmation not accepted")
			}
			if !persist && (d != nil || result.RequeueAfter == 0 || client.graphWrites != 0) {
				t.Fatal("unknown create must requeue without replay")
			}
		})
	}
}

func TestBuildGraphEmptyAndOwner(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	graph := client.SeedBuildGraph(bi, map[string]ebsv1.DcgNodeState{})
	round := &reconcileRound{key: cacheKey(bi.Namespace, bi.Name), current: bi}
	d, _, _, err := c.loadGraph(context.Background(), round)
	if err != nil || d == nil || d.Len() != 0 {
		t.Fatal("empty persisted graph must be loaded", err)
	}
	graph.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&ebsv1.BuildInfo{ObjectMeta: metav1.ObjectMeta{Name: bi.Name, UID: "other"}}, ebsv1.SchemeGroupVersion.WithKind("BuildInfo"))}
	client.graphs[round.key] = graph
	d, _, _, err = c.loadGraph(context.Background(), round)
	if err != nil || d != nil || findCondition(round.current.Status.Conditions, conditionBuildGraphUnavailable).Reason != "BuildGraphOwnerMismatch" {
		t.Fatal("wrong owner must stop processing")
	}
}

func TestBuildGraphUpdateConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome clientpkg.WriteOutcome
		code    int
		persist bool
	}{
		{name: "confirmed", outcome: clientpkg.WriteUnknown, persist: true},
		{name: "unconfirmed", outcome: clientpkg.WriteUnknown},
		{name: "conflict", outcome: clientpkg.WriteRejected, code: 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			bi := client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoProcessing))
			client.SeedBuildGraph(bi, dcgStateAB())
			round := &reconcileRound{key: cacheKey(bi.Namespace, bi.Name), current: bi}
			dcg, _, _, err := c.loadGraph(context.Background(), round)
			if err != nil {
				t.Fatal(err)
			}
			candidate := dcg.Clone()
			candidate.AddInstallEdge("a", "b", ebsv1.VersionConstraint{})
			candidate.RefreshCyclesAndBreaks()
			client.injectedWrites["update-graph"] = &injectedWrite{
				outcome: tc.outcome, statusCode: tc.code, persist: tc.persist,
			}
			got, result, err := c.writeGraph(context.Background(), round, candidate)
			if err != nil {
				t.Fatal(err)
			}
			stored := testGraph(t, client, bi)
			if tc.persist {
				if got == nil || result.RequeueAfter != 0 || !dcgStateMatches(stored.Status.Nodes, candidate.ToState()) {
					t.Fatal("confirmed graph update not accepted")
				}
			} else if got != nil || result.RequeueAfter == 0 || !dcgStateMatches(stored.Status.Nodes, dcg.ToState()) {
				t.Fatal("unconfirmed candidate must be discarded without replay")
			}
		})
	}
}

func TestBuildGraphAlreadyExistsUsesPersistedGraph(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	stored := client.SeedBuildGraph(bi, dcgStateAB())
	round := &reconcileRound{key: cacheKey(bi.Namespace, bi.Name), current: bi}
	got, result, err := c.writeGraph(context.Background(), round, DcgDictFromState(map[string]ebsv1.DcgNodeState{}))
	if err != nil || result.RequeueAfter != 0 || got == nil || !dcgStateMatches(got.ToState(), stored.Status.Nodes) {
		t.Fatalf("must adopt existing graph, not the newly computed candidate: %v %v", result, err)
	}
	// A fresh controller must load the graph even when the BuildInfo status write never happened.
	c.dcgDict.Invalidate(round.key)
	round.graph = nil
	got, _, result, err = c.loadGraph(context.Background(), round)
	if err != nil || result.RequeueAfter != 0 || got == nil || !dcgStateMatches(got.ToState(), stored.Status.Nodes) {
		t.Fatalf("persisted graph not recovered: %v %v", result, err)
	}
}

func TestInitWaitsForConfirmedBuildGraph(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	client.injectedWrites["create-graph"] = &injectedWrite{outcome: clientpkg.WriteUnknown}
	result, err := c.reconcile(context.Background(), testNS+"/"+testBuild)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("graph confirmation must requeue: %v %v", result, err)
	}
	info := getBuildInfo(t, client)
	if len(listJobs(t, client)) != 0 || info.Status.SpecStatus.Len() != 0 {
		t.Fatal("graph must be persisted before spec status or Jobs")
	}
	reconcileOnce(t, c)
	info = getBuildInfo(t, client)
	if len(listJobs(t, client)) != 1 || len(testGraph(t, client, info).Status.Nodes) != 1 {
		t.Fatal("dispatch did not resume after graph creation")
	}
}

func TestBuildGraphOverlappingEdges(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	round := &reconcileRound{key: cacheKey(bi.Namespace, bi.Name), current: bi}
	candidate := DcgDictFromState(dcgStateAB())
	candidate.AddInstallEdge("b", "a", ebsv1.VersionConstraint{})
	got, result, err := c.writeGraph(context.Background(), round, candidate)
	if err != nil || result.RequeueAfter != 0 || got == nil || !dcgStateMatches(got.ToState(), candidate.ToState()) {
		t.Fatalf("overlapping build/install edges must round-trip with multiplicity: %v %v", result, err)
	}
}
