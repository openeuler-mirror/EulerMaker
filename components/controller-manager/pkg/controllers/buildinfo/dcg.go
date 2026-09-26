package buildinfo

import (
	"container/heap"
	"sort"

	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	ebsv1 "ebs-api/ebs/v1"
)

// dcg.go implements the in-memory spec dependency graph (design 7.2.1/15.9):
// DcgDict/DcgNode, Kosaraju SCC cycle detection, iterative-peeling break-point
// selection, DispatchRequirements, deterministic SortedNodes traversal, state
// round-trip (G-02/G-09), and runtime install edge-append support (7.4.7).

// DcgNode mirrors ebsv1.DcgNodeState (design 15.9). Version defaults to "NA"
// when empty (normalized by ToState).
type DcgNode struct {
	Version        string
	OutDep         []string
	InDep          map[string]ebsv1.VersionConst
	InstallInDep   map[string]ebsv1.VersionConst
	BootstrapBreak bool
}

// InDegree returns the merged build/install in-degree (design 15.9: no
// separate inDegree field).
func (n *DcgNode) InDegree() int { return len(n.InDep) + len(n.InstallInDep) }

// DcgDict is the in-memory spec dependency graph (design 7.2.1). cycleNodes is
// derived state: a pure function of the edge set, recomputed lazily via
// ensureCycleDetection and after runtime edge appends; bootstrap break points
// are persisted on the nodes (BootstrapBreak) and never re-selected on load
// (G-09).
type DcgDict struct {
	nodes      map[string]*DcgNode
	cycleNodes map[string]struct{}
}

// NewDcgDict builds the initial graph: runs SCC decomposition and selects
// bootstrap break points (design 7.2.1 iterative peeling), marking them on the
// nodes. nodes is adopted, not copied.
func NewDcgDict(nodes map[string]*DcgNode) *DcgDict {
	d := &DcgDict{nodes: nodes}
	for _, pick := range d.selectBreaks(nil) {
		d.nodes[pick].BootstrapBreak = true
	}
	d.refreshCycleNodes()
	return d
}

// DcgDictFromState loads a persisted graph (G-02). cycleNodes is recomputed
// lazily; bootstrap break points are read from the persisted marks and never
// re-selected (G-09).
func DcgDictFromState(state map[string]ebsv1.DcgNodeState) *DcgDict {
	nodes := make(map[string]*DcgNode, len(state))
	for name, st := range state {
		nodes[name] = &DcgNode{
			Version:        st.Version,
			OutDep:         append([]string(nil), st.OutDep...),
			InDep:          cloneVersionConstMap(st.InDep),
			InstallInDep:   cloneVersionConstMap(st.InstallInDep),
			BootstrapBreak: st.BootstrapBreak,
		}
	}
	return &DcgDict{nodes: nodes}
}

// ToState exports the graph for persistence to BuildInfo.status.dcg (G-02).
func (d *DcgDict) ToState() map[string]ebsv1.DcgNodeState {
	out := make(map[string]ebsv1.DcgNodeState, len(d.nodes))
	for name, n := range d.nodes {
		version := n.Version
		if version == "" {
			version = "NA"
		}
		out[name] = ebsv1.DcgNodeState{
			Version:        version,
			OutDep:         append([]string(nil), n.OutDep...),
			InDep:          cloneVersionConstMap(n.InDep),
			InstallInDep:   cloneVersionConstMap(n.InstallInDep),
			BootstrapBreak: n.BootstrapBreak,
		}
	}
	return out
}

// Clone deep-copies the graph for candidate computations (7.4.7: edge appends
// never touch the live graph before persistence).
func (d *DcgDict) Clone() *DcgDict {
	nodes := make(map[string]*DcgNode, len(d.nodes))
	for name, n := range d.nodes {
		nodes[name] = &DcgNode{
			Version:        n.Version,
			OutDep:         append([]string(nil), n.OutDep...),
			InDep:          cloneVersionConstMap(n.InDep),
			InstallInDep:   cloneVersionConstMap(n.InstallInDep),
			BootstrapBreak: n.BootstrapBreak,
		}
	}
	return &DcgDict{nodes: nodes}
}

// Node returns the node for name, or nil.
func (d *DcgDict) Node(name string) *DcgNode { return d.nodes[name] }

// Len returns the node count.
func (d *DcgDict) Len() int { return len(d.nodes) }

// Names returns all node names in deterministic (dictionary) order.
func (d *DcgDict) Names() []string {
	names := make([]string, 0, len(d.nodes))
	for name := range d.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsCycleNode reports whether name is on a dependency cycle (design 7.2.1:
// cycleNodes = members of SCCs with size > 1, plus self-loop nodes).
func (d *DcgDict) IsCycleNode(name string) bool {
	d.ensureCycleDetection()
	_, ok := d.cycleNodes[name]
	return ok
}

// GetBootstrapBreaks returns the break-point set: deduplicated, dictionary
// ordered, read from the persisted node marks (design 15.9).
func (d *DcgDict) GetBootstrapBreaks() []string {
	var breaks []string
	for name, n := range d.nodes {
		if n.BootstrapBreak {
			breaks = append(breaks, name)
		}
	}
	sort.Strings(breaks)
	return breaks
}

// DispatchRequirements returns the required dispatch count per spec: 2 for
// cycle nodes, 1 for normal nodes (design 15.9).
func (d *DcgDict) DispatchRequirements() map[string]int64 {
	d.ensureCycleDetection()
	out := make(map[string]int64, len(d.nodes))
	for name := range d.nodes {
		if _, ok := d.cycleNodes[name]; ok {
			out[name] = 2
		} else {
			out[name] = 1
		}
	}
	return out
}

// SortedNodes returns a deterministic topological-style traversal order used by
// advanceDownstream (7.3 step 4): Kahn over the merged build/install graph,
// dictionary-smallest first among ready nodes; on cycle remainder the
// dictionary-smallest unvisited node is force-emitted. Upstreams precede
// downstreams wherever the edge set allows.
func (d *DcgDict) SortedNodes() []string {
	indeg := make(map[string]int, len(d.nodes))
	ready := &stringHeap{}
	for name, n := range d.nodes {
		indeg[name] = n.InDegree()
		if indeg[name] == 0 {
			heap.Push(ready, name)
		}
	}
	out := make([]string, 0, len(d.nodes))
	emitted := make(map[string]bool, len(d.nodes))
	for len(out) < len(d.nodes) {
		if ready.Len() == 0 {
			// Cycle remainder: force-emit the dictionary-smallest unvisited node.
			for _, name := range d.Names() {
				if !emitted[name] {
					heap.Push(ready, name)
					break
				}
			}
		}
		name := heap.Pop(ready).(string)
		if emitted[name] {
			continue
		}
		emitted[name] = true
		out = append(out, name)
		for _, down := range d.nodes[name].OutDep {
			if _, ok := d.nodes[down]; !ok {
				continue
			}
			indeg[down]--
			if indeg[down] == 0 {
				heap.Push(ready, down)
			}
		}
	}
	return out
}

// AddInstallEdge appends a runtime install edge spec -> provider on this
// (candidate) graph (7.4.7): installInDep[spec][provider] = vc and
// outDep[provider] += spec, in- and out-degree updated together. Idempotent:
// returns false when the edge already exists or either endpoint is missing.
func (d *DcgDict) AddInstallEdge(spec, provider string, vc ebsv1.VersionConst) bool {
	s, p := d.nodes[spec], d.nodes[provider]
	if s == nil || p == nil {
		return false
	}
	if _, ok := s.InstallInDep[provider]; ok {
		return false
	}
	if s.InstallInDep == nil {
		s.InstallInDep = map[string]ebsv1.VersionConst{}
	}
	s.InstallInDep[provider] = vc
	p.OutDep = append(p.OutDep, spec)
	sort.Strings(p.OutDep)
	return true
}

// RefreshCyclesAndBreaks recomputes cycles on the (candidate) graph after
// runtime edge appends and appends break points for newly appeared cycles
// (7.4.7): existing BootstrapBreak marks are treated as already removed for
// the peeling computation and are never re-selected (G-09). Returns the newly
// added break points (dictionary ordered).
func (d *DcgDict) RefreshCyclesAndBreaks() []string {
	preRemoved := map[string]bool{}
	for name, n := range d.nodes {
		if n.BootstrapBreak {
			preRemoved[name] = true
		}
	}
	picks := d.selectBreaks(preRemoved)
	for _, pick := range picks {
		d.nodes[pick].BootstrapBreak = true
	}
	d.refreshCycleNodes()
	sort.Strings(picks)
	return picks
}

// BuildDcgNodes constructs the node map of the initial graph (design 16.1).
// Build edges come from buildRequires (buildRemoves excluded), install edges
// from the merged install dependency set (explicit SpecDepend.requires ∪
// RpmRepo-layer rpm requires, intersection merge). Both run the same layered
// selection chain; only providers inside the build set create edges. A build
// self-provide stays as a self-loop (E-14, broken by the peel pick); an
// install self-provide is filtered (same-spec subpackage dependencies are
// self-consistent within one build). OutDep is a multiset aligned with the
// merged in-degree: one entry per InDep/InstallInDep entry.
func BuildDcgNodes(buildSet map[string]specparse.SpecDepend, sources *rpmver.RpmMetaSources, prefer []string) map[string]*DcgNode {
	names := make([]string, 0, len(buildSet))
	for name := range buildSet {
		names = append(names, name)
	}
	sort.Strings(names)
	nodes := make(map[string]*DcgNode, len(buildSet))
	for _, name := range names {
		nodes[name] = &DcgNode{
			Version:      buildSet[name].Version,
			InDep:        map[string]ebsv1.VersionConst{},
			InstallInDep: map[string]ebsv1.VersionConst{},
		}
	}
	addEdge := func(spec, provider string, vc ebsv1.VersionConst, install bool) {
		node := nodes[spec]
		if install {
			if _, dup := node.InstallInDep[provider]; dup {
				return
			}
			node.InstallInDep[provider] = vc
		} else {
			if _, dup := node.InDep[provider]; dup {
				return
			}
			node.InDep[provider] = vc
		}
		nodes[provider].OutDep = append(nodes[provider].OutDep, spec)
	}
	for _, name := range names {
		depend := buildSet[name]
		for _, reqName := range sortedConstKeys(depend.BuildRequires, depend.BuildRemoves) {
			vc := depend.BuildRequires[reqName]
			selection, ok := sources.FindProvider(reqName, vc, prefer)
			if !ok {
				continue // miss: no edge; 7.4.1 condition 2 owns the verdict
			}
			if _, inSet := buildSet[selection.Provider.SpecName]; !inSet {
				continue // bootstrap/external provider: availability evidence only
			}
			addEdge(name, selection.Provider.SpecName, vc, false)
		}
		installDeps := mergedInstallDeps(depend.Requires, sources.RepoRequires(name))
		for _, reqName := range sortedConstKeys(installDeps, nil) {
			vc := installDeps[reqName]
			selection, ok := sources.FindProvider(reqName, vc, prefer)
			if !ok {
				continue // miss: no edge and no dispatch gate (16.1 install 3)
			}
			provider := selection.Provider.SpecName
			if provider == name {
				continue // same-spec subpackage self-dependency: filtered
			}
			if _, inSet := buildSet[provider]; !inSet {
				continue
			}
			addEdge(name, provider, vc, true)
		}
	}
	for _, node := range nodes {
		sort.Strings(node.OutDep)
	}
	return nodes
}

// sortedConstKeys returns the constraint-map keys in dictionary order,
// excluding keys present in exclude (buildRemoves; nil for no exclusions).
func sortedConstKeys(constraints map[string]ebsv1.VersionConst, exclude map[string]ebsv1.VersionConst) []string {
	keys := make([]string, 0, len(constraints))
	for key := range constraints {
		if _, skipped := exclude[key]; skipped {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// mergedInstallDeps computes a spec's install dependency set (design 16.1):
// the explicit SpecDepend.requires ∪ the spec's own RpmRepo-layer rpm
// requires. Same-name entries merge per field so both constraints apply; a
// field conflict resolves to the RpmMeta.requires value.
func mergedInstallDeps(explicit, rpm map[string]ebsv1.VersionConst) map[string]ebsv1.VersionConst {
	if len(explicit) == 0 && len(rpm) == 0 {
		return nil
	}
	out := make(map[string]ebsv1.VersionConst, len(explicit)+len(rpm))
	for name, vc := range explicit {
		out[name] = vc
	}
	for name, vc := range rpm {
		if existing, ok := out[name]; ok {
			out[name] = intersectConst(existing, vc)
			continue
		}
		out[name] = vc
	}
	return out
}

// intersectConst merges two constraints on the same dependency name field by
// field: a field present in only one side is kept (both constraints apply,
// the intersection tightens); a conflicting field resolves to the
// RpmMeta.requires value (design 16.1).
func intersectConst(explicit, rpm ebsv1.VersionConst) ebsv1.VersionConst {
	merge := func(x, y string) string {
		if x == "" {
			return y
		}
		if y == "" || x == y {
			return x
		}
		return y
	}
	return ebsv1.VersionConst{
		GT: merge(explicit.GT, rpm.GT),
		GE: merge(explicit.GE, rpm.GE),
		EQ: merge(explicit.EQ, rpm.EQ),
		LE: merge(explicit.LE, rpm.LE),
		LT: merge(explicit.LT, rpm.LT),
	}
}

// ensureCycleDetection lazily recomputes cycleNodes from the current edge set
// (design 15.9: SCC is a pure function of the edge set).
func (d *DcgDict) ensureCycleDetection() {
	if d.cycleNodes != nil {
		return
	}
	d.refreshCycleNodes()
}

func (d *DcgDict) refreshCycleNodes() {
	cycle := map[string]struct{}{}
	for _, members := range d.sccs() {
		if len(members) > 1 {
			for _, m := range members {
				cycle[m] = struct{}{}
			}
			continue
		}
		// Single-node SCC: cycle only on a self-loop (outDep contains itself).
		m := members[0]
		for _, down := range d.nodes[m].OutDep {
			if down == m {
				cycle[m] = struct{}{}
				break
			}
		}
	}
	d.cycleNodes = cycle
}

// sccs returns the strongly connected components of the merged build/install
// graph via Kosaraju (design 7.2.1), each component's members dictionary
// ordered, components ordered by their smallest member name.
func (d *DcgDict) sccs() [][]string {
	names := d.Names()
	succ := make(map[string][]string, len(d.nodes))
	pred := make(map[string][]string, len(d.nodes))
	for _, name := range names {
		for _, down := range d.nodes[name].OutDep {
			if _, ok := d.nodes[down]; !ok {
				continue
			}
			succ[name] = append(succ[name], down)
			pred[down] = append(pred[down], name)
		}
	}
	for _, name := range names {
		sort.Strings(succ[name])
		sort.Strings(pred[name])
	}

	// Pass 1: iterative DFS on G, record finish order.
	visited := make(map[string]bool, len(d.nodes))
	finish := make([]string, 0, len(d.nodes))
	type frame struct {
		node string
		next int
	}
	for _, start := range names {
		if visited[start] {
			continue
		}
		visited[start] = true
		stack := []frame{{node: start}}
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			if f.next < len(succ[f.node]) {
				v := succ[f.node][f.next]
				f.next++
				if !visited[v] {
					visited[v] = true
					stack = append(stack, frame{node: v})
				}
				continue
			}
			finish = append(finish, f.node)
			stack = stack[:len(stack)-1]
		}
	}

	// Pass 2: DFS on G^T in reverse finish order.
	assigned := make(map[string]bool, len(d.nodes))
	var sccs [][]string
	for i := len(finish) - 1; i >= 0; i-- {
		start := finish[i]
		if assigned[start] {
			continue
		}
		assigned[start] = true
		members := []string{start}
		stack := []string{start}
		for len(stack) > 0 {
			u := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, v := range pred[u] {
				if !assigned[v] {
					assigned[v] = true
					members = append(members, v)
					stack = append(stack, v)
				}
			}
		}
		sort.Strings(members)
		sccs = append(sccs, members)
	}
	sort.Slice(sccs, func(i, j int) bool { return sccs[i][0] < sccs[j][0] })
	return sccs
}

// selectBreaks runs the iterative-peeling break-point selection (design
// 7.2.1): SCCs are processed in order of their smallest member name; within an
// SCC, incremental Kahn over intra-SCC edges advances to deadlock, then the
// unprocessed node with the largest outDep (tie: dictionary-largest spec name)
// is picked, and peeling continues until the whole SCC is processed.
// preRemoved marks nodes treated as already peeled (existing break points on
// runtime refresh, G-09); they are never picked again. Bookkeeping only: the
// actual graph is not modified.
func (d *DcgDict) selectBreaks(preRemoved map[string]bool) []string {
	var picks []string
	for _, members := range d.sccs() {
		picks = append(picks, d.peelSCC(members, preRemoved)...)
	}
	return picks
}

func (d *DcgDict) peelSCC(members []string, preRemoved map[string]bool) []string {
	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		memberSet[m] = true
	}
	processed := make(map[string]bool, len(members))
	remaining := 0
	for _, m := range members {
		if preRemoved[m] {
			processed[m] = true
			continue
		}
		remaining++
	}
	// In-degree counting intra-SCC edges from not-yet-processed nodes only
	// (pre-removed nodes are already peeled, so their edges are gone).
	indeg := make(map[string]int, remaining)
	ready := &stringHeap{}
	for _, m := range members {
		if processed[m] {
			continue
		}
		degree := 0
		for up := range d.nodes[m].InDep {
			if memberSet[up] && !processed[up] {
				degree++
			}
		}
		for up := range d.nodes[m].InstallInDep {
			if memberSet[up] && !processed[up] {
				degree++
			}
		}
		indeg[m] = degree
		if degree == 0 {
			heap.Push(ready, m)
		}
	}
	var picks []string
	done := 0
	for done < remaining {
		var name string
		if ready.Len() > 0 {
			name = heap.Pop(ready).(string)
		} else {
			// Deadlock: pick the unprocessed node with the largest outDep;
			// tie broken by the dictionary-largest spec name. members is
			// dictionary ordered, so the scan is deterministic.
			best := ""
			for _, candidate := range members {
				if processed[candidate] {
					continue
				}
				if best == "" {
					best = candidate
					continue
				}
				co, bo := len(d.nodes[candidate].OutDep), len(d.nodes[best].OutDep)
				if co > bo || (co == bo && candidate > best) {
					best = candidate
				}
			}
			name = best
			picks = append(picks, name)
		}
		if processed[name] {
			continue
		}
		processed[name] = true
		done++
		for _, down := range d.nodes[name].OutDep {
			if !memberSet[down] || processed[down] {
				continue
			}
			indeg[down]--
			if indeg[down] == 0 {
				heap.Push(ready, down)
			}
		}
	}
	return picks
}

func cloneVersionConstMap(in map[string]ebsv1.VersionConst) map[string]ebsv1.VersionConst {
	if in == nil {
		return nil
	}
	out := make(map[string]ebsv1.VersionConst, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// stringHeap is a min-heap of strings in dictionary order.
type stringHeap []string

func (h stringHeap) Len() int            { return len(h) }
func (h stringHeap) Less(i, j int) bool  { return h[i] < h[j] }
func (h stringHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *stringHeap) Push(x interface{}) { *h = append(*h, x.(string)) }
func (h *stringHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
