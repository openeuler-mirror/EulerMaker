package build

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
)

var errUnknown = errors.New("response lost")

// Shared fake ES provides atomic document operations, not a process-local
// admission lock. Each manager below represents an independent apiserver.
type coordinationFake struct {
	mu                sync.Mutex
	docs              map[string]es.Hit
	builds            map[string]*ebsv1.Build
	seq               int64
	createUnknown     string
	transitionUnknown string
}

func newCoordinationFake() *coordinationFake {
	return &coordinationFake{docs: map[string]es.Hit{}, builds: map[string]*ebsv1.Build{}}
}
func (f *coordinationFake) Create(ctx context.Context, r, id string, d es.Document) (es.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.docs[r+"/"+id]; ok {
		return es.Version{}, &es.HTTPError{StatusCode: 409}
	}
	f.seq++
	f.docs[r+"/"+id] = es.Hit{ID: id, Document: d, SeqNo: f.seq, PrimaryTerm: 1}
	if f.createUnknown == r {
		return es.Version{}, errUnknown
	}
	return es.Version{SeqNo: f.seq, PrimaryTerm: 1}, nil
}
func (f *coordinationFake) Get(ctx context.Context, r, id string) (*es.Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.docs[r+"/"+id]
	if !ok {
		return nil, &es.HTTPError{StatusCode: 404}
	}
	return &h, nil
}
func (f *coordinationFake) Update(ctx context.Context, r, id string, d es.Document, seq, term int64) (es.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.docs[r+"/"+id]
	if !ok || h.SeqNo != seq || h.PrimaryTerm != term {
		return es.Version{}, &es.HTTPError{StatusCode: 409}
	}
	f.seq++
	h.Document = d
	h.SeqNo = f.seq
	f.docs[r+"/"+id] = h
	var c claim
	_ = json.Unmarshal(d.Data, &c)
	if f.transitionUnknown == c.State {
		return es.Version{}, errUnknown
	}
	return es.Version{SeqNo: f.seq, PrimaryTerm: 1}, nil
}
func (f *coordinationFake) Delete(ctx context.Context, r, id string, seq, term int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.docs[r+"/"+id]
	if !ok {
		return &es.HTTPError{StatusCode: 404}
	}
	if h.SeqNo != seq || h.PrimaryTerm != term {
		return &es.HTTPError{StatusCode: 409}
	}
	delete(f.docs, r+"/"+id)
	return nil
}
func (f *coordinationFake) OpenPIT(context.Context, string, string) (string, error) {
	return "pit", nil
}
func (f *coordinationFake) ClosePIT(context.Context, string) error { return nil }
func (f *coordinationFake) SearchPIT(_ context.Context, _ string, _ string, _ map[string]interface{}, size int64, after []json.RawMessage) (*es.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cursor := ""
	if len(after) > 0 {
		_ = json.Unmarshal(after[0], &cursor)
	}
	var hits []es.Hit
	for k, h := range f.docs {
		if len(k) > len(claimResource) && k[:len(claimResource)+1] == claimResource+"/" && h.ID > cursor {
			hits = append(hits, h)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ID < hits[j].ID })
	if len(hits) > int(size) {
		hits = hits[:size]
	}
	for i := range hits {
		v, _ := json.Marshal(hits[i].ID)
		hits[i].Sort = []json.RawMessage{v}
	}
	return &es.SearchResult{Hits: hits, PITID: "pit"}, nil
}

type coordinationBuilds struct {
	rest.Lister
	f *coordinationFake
}

func (s coordinationBuilds) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	ns, _ := request.NamespaceFrom(ctx)
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	b := s.f.builds[ns+"/"+name]
	if b == nil {
		return nil, apierrors.NewNotFound(ebsv1.Resource("builds"), name)
	}
	return b.DeepCopy(), nil
}
func (s coordinationBuilds) List(ctx context.Context, opts *internalversion.ListOptions) (runtime.Object, error) {
	ns, _ := request.NamespaceFrom(ctx)
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	list := &ebsv1.BuildList{}
	for _, b := range s.f.builds {
		if b.Namespace == ns && opts.LabelSelector.Matches(labels.Set(b.Labels)) && (opts.FieldSelector == nil || opts.FieldSelector.Matches(fields.Set{"status.phase": string(b.Status.Phase)})) {
			list.Items = append(list.Items, *b.DeepCopy())
		}
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name > list.Items[j].Name })
	if len(list.Items) > int(opts.Limit) {
		list.Items = list.Items[:opts.Limit]
	}
	return list, nil
}
func testManager(f *coordinationFake) *claimManager {
	s := coordinationBuilds{f: f}
	return newClaimManager(f, s, s)
}
func testBuild(name, typ string) *ebsv1.Build {
	return &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project-a", UID: types.UID("uid-" + name), Labels: map[string]string{ebsv1.BuildTargetOSLabel: "os", ebsv1.BuildTargetArchLabel: "arch", ebsv1.BuildTypeLabel: typ}}, Spec: ebsv1.BuildSpec{BuildType: typ, BuildTarget: ebsv1.BuildTarget{Os: "os", Arch: "arch"}}, Status: ebsv1.BuildStatus{Phase: ebsv1.BuildPending}}
}
func (f *coordinationFake) persist(b *ebsv1.Build) func() (runtime.Object, error) {
	return func() (runtime.Object, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := b.Namespace + "/" + b.Name
		if f.builds[key] != nil {
			return nil, apierrors.NewAlreadyExists(ebsv1.Resource("builds"), b.Name)
		}
		f.builds[key] = b.DeepCopy()
		return b.DeepCopy(), nil
	}
}

func TestClaimConcurrentServers(t *testing.T) {
	f := newCoordinationFake()
	m1, m2 := testManager(f), testManager(f)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, m := range []*claimManager{m1, m2} {
		b := testBuild([]string{"a", "b"}[i], "full")
		go func(m *claimManager, b *ebsv1.Build) {
			<-start
			_, err := m.create(context.Background(), b, false, f.persist(b))
			results <- err
		}(m, b)
	}
	close(start)
	success, conflict := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else if apierrors.IsConflict(err) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	if len(f.builds) != 1 {
		t.Fatal("multiple Builds admitted")
	}
}

func TestClaimTargetsAndSingle(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	archBaseline := testBuild("arch-baseline", "full")
	archBaseline.Spec.BuildTarget.Arch = "other"
	archBaseline.Labels[ebsv1.BuildTargetArchLabel] = "other"
	archBaseline.Status.Phase = ebsv1.BuildSuccess
	f.builds[archBaseline.Namespace+"/"+archBaseline.Name] = archBaseline
	projectBaseline := testBuild("project-baseline", "full")
	projectBaseline.Namespace = "other-project"
	projectBaseline.Status.Phase = ebsv1.BuildSuccess
	f.builds[projectBaseline.Namespace+"/"+projectBaseline.Name] = projectBaseline
	for i, b := range []*ebsv1.Build{testBuild("a", "full"), testBuild("b", "single"), testBuild("c", "incremental"), testBuild("d", "specified")} {
		if i == 2 {
			b.Spec.BuildTarget.Arch = "other"
			b.Labels[ebsv1.BuildTargetArchLabel] = "other"
		}
		if i == 3 {
			b.Namespace = "other-project"
		}
		if _, err := m.create(context.Background(), b, false, f.persist(b)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMissingFullBuildReleasesClaim(t *testing.T) {
	for _, typ := range []string{"incremental", "specified"} {
		t.Run(typ, func(t *testing.T) {
			f := newCoordinationFake()
			m := testManager(f)
			b := testBuild("new", typ)
			_, err := m.create(context.Background(), b, false, f.persist(b))
			status, ok := err.(*apierrors.StatusError)
			if !ok || status.ErrStatus.Reason != "FullBuildRequired" {
				t.Fatalf("expected missing full Build, got %v", err)
			}
			if len(f.docs) != 0 || len(f.builds) != 0 {
				t.Fatal("rejected create left an occupied target or Build")
			}
		})
	}
}

func TestSingleCreatesNoCoordinationRecords(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	b := testBuild("single", "single")
	if _, err := m.create(context.Background(), b, false, f.persist(b)); err != nil {
		t.Fatal(err)
	}
	if len(f.docs) != 0 {
		t.Fatal("single Build must not create coordination records")
	}
}

func TestExistingBuildNameStillRejected(t *testing.T) {
	for _, typ := range []string{"single", "full"} {
		t.Run(typ, func(t *testing.T) {
			f := newCoordinationFake()
			m := testManager(f)
			b := testBuild("existing", typ)
			f.builds[b.Namespace+"/"+b.Name] = b
			if _, err := m.create(context.Background(), b, false, func() (runtime.Object, error) {
				t.Fatal("duplicate Build must not be persisted")
				return nil, nil
			}); !apierrors.IsAlreadyExists(err) {
				t.Fatalf("expected AlreadyExists, got %v", err)
			}
			if len(f.docs) != 0 {
				t.Fatal("duplicate Build must not create coordination records")
			}
		})
	}
}

func TestClaimDryRunAndHistory(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	b := testBuild("new", "full")
	called := false
	if _, err := m.create(context.Background(), b, true, func() (runtime.Object, error) { called = true; return b, nil }); err != nil {
		t.Fatal(err)
	}
	if !called || len(f.docs) != 0 || len(f.builds) != 0 {
		t.Fatal("dry run mutated storage")
	}
	old := testBuild("old", "incremental")
	f.builds[old.Namespace+"/"+old.Name] = old
	if _, err := m.create(context.Background(), b, false, f.persist(b)); !apierrors.IsConflict(err) {
		t.Fatalf("want historical conflict: %v", err)
	}
	if len(f.docs) != 0 {
		t.Fatal("historical rejection leaked reservation")
	}
	old.Status.Phase = ebsv1.BuildFailed
	if _, err := m.create(context.Background(), b, false, f.persist(b)); err != nil {
		t.Fatal(err)
	}
}

func TestClaimWriteUnknown(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-visible", true: "committed"}[committed], func(t *testing.T) {
			f := newCoordinationFake()
			m := testManager(f)
			b := testBuild("a", "full")
			calls := 0
			result, err := m.create(context.Background(), b, false, func() (runtime.Object, error) {
				calls++
				if committed {
					_, _ = f.persist(b)()
				}
				return nil, errUnknown
			})
			if calls != 1 {
				t.Fatal("write replayed")
			}
			if committed {
				if err != nil || result == nil {
					t.Fatalf("confirmation failed: %v", err)
				}
			} else if err != errUnknown {
				t.Fatal(err)
			}
			c, getErr := m.get(context.Background(), coordinationID(b.Namespace, "os", "arch"))
			if getErr != nil {
				t.Fatal(getErr)
			}
			want := creating
			if committed {
				want = bound
			}
			if c.State != want {
				t.Fatalf("state=%s", c.State)
			}
			for i := 0; i < 2; i++ {
				if err := m.scan(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if !committed {
				if _, err := m.create(context.Background(), testBuild("b", "full"), false, f.persist(testBuild("b", "full"))); !apierrors.IsConflict(err) {
					t.Fatalf("unknown write unblocked: %v", err)
				}
			}
		})
	}
}

func TestClaimCASUnknownNeverSendsBuild(t *testing.T) {
	f := newCoordinationFake()
	f.transitionUnknown = creating
	m := testManager(f)
	b := testBuild("a", "full")
	_, err := m.create(context.Background(), b, false, func() (runtime.Object, error) { t.Fatal("Build sent after unknown CAS"); return nil, nil })
	if err == nil {
		t.Fatal("expected error")
	}
	if err := m.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err := m.get(context.Background(), coordinationID(b.Namespace, "os", "arch"))
	if err != nil || c.State != creating {
		t.Fatalf("lost unknown claim: %v", err)
	}
}

func TestClaimReservationUnknownConfirmed(t *testing.T) {
	f := newCoordinationFake()
	f.createUnknown = claimResource
	m := testManager(f)
	b := testBuild("a", "full")
	if _, err := m.create(context.Background(), b, false, f.persist(b)); err != nil {
		t.Fatal(err)
	}
}

func TestClaimReservedRecoveryFencesSender(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	c, err := m.acquire(context.Background(), testBuild("a", "full"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.transition(context.Background(), c, creating, ""); !es.IsStatus(err, 409) {
		t.Fatalf("stale sender not fenced: %v", err)
	}
}

func TestClaimTerminalDeleteAndStaleCleanup(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "deleted"}[deleted], func(t *testing.T) {
			f := newCoordinationFake()
			m := testManager(f)
			b := testBuild("a", "full")
			if _, err := m.create(context.Background(), b, false, f.persist(b)); err != nil {
				t.Fatal(err)
			}
			if deleted {
				delete(f.builds, b.Namespace+"/"+b.Name)
			} else {
				f.builds[b.Namespace+"/"+b.Name].Status.Phase = ebsv1.BuildAborted
				b.Status.Phase = ebsv1.BuildAborted
			}
			m.afterWrite(context.Background(), b, deleted)
			if _, err := m.get(context.Background(), coordinationID(b.Namespace, "os", "arch")); !es.IsStatus(err, 404) {
				t.Fatalf("claim not released: %v", err)
			}
			next := testBuild("b", "full")
			if _, err := m.create(context.Background(), next, false, f.persist(next)); err != nil {
				t.Fatal(err)
			}
			m.afterWrite(context.Background(), b, true)
			c, err := m.get(context.Background(), coordinationID(b.Namespace, "os", "arch"))
			if err != nil || c.BuildName != "b" {
				t.Fatalf("old cleanup deleted new owner: %v", err)
			}
		})
	}
}

func TestClaimCleanupAfterCrash(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	b := testBuild("a", "full")
	if _, err := m.create(context.Background(), b, false, f.persist(b)); err != nil {
		t.Fatal(err)
	}
	f.builds[b.Namespace+"/"+b.Name].Status.Phase = ebsv1.BuildSuccess
	other := testManager(f)
	if err := other.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.get(context.Background(), coordinationID(b.Namespace, "os", "arch")); !es.IsStatus(err, 404) {
		t.Fatal("leaked claim")
	}
}

func TestClaimConfirmedRejectionReleases(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	b := testBuild("a", "full")
	_, err := m.create(context.Background(), b, false, func() (runtime.Object, error) { return nil, apierrors.NewBadRequest("rejected") })
	if !apierrors.IsBadRequest(err) {
		t.Fatal(err)
	}
	if _, err := m.get(context.Background(), coordinationID(b.Namespace, "os", "arch")); !es.IsStatus(err, 404) {
		t.Fatal("rejected create retained claim")
	}
	next := testBuild("b", "full")
	if _, err := m.create(context.Background(), next, false, f.persist(next)); err != nil {
		t.Fatalf("released target cannot admit next Build: %v", err)
	}
}

func TestClaimUsesBuildNameWithoutUID(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	b := testBuild("a", "full")
	b.UID = ""
	if _, err := m.create(context.Background(), b, false, f.persist(b)); err != nil {
		t.Fatal(err)
	}
	c, err := m.get(context.Background(), coordinationID(b.Namespace, "os", "arch"))
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]interface{}
	if err := json.Unmarshal(c.document().Data, &data); err != nil {
		t.Fatal(err)
	}
	if _, exists := data["buildUID"]; exists {
		t.Fatal("claim must not store buildUID")
	}
	if data["buildName"] != b.Name {
		t.Fatal("claim must store buildName")
	}
	f.builds[b.Namespace+"/"+b.Name].Status.Phase = ebsv1.BuildSuccess
	if err := testManager(f).scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.get(context.Background(), c.id()); !es.IsStatus(err, 404) {
		t.Fatalf("recovery must release terminal Build by name: %v", err)
	}
}

func TestClaimIdentityMismatchRetained(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	b := testBuild("a", "full")
	if _, err := m.create(context.Background(), b, false, f.persist(b)); err != nil {
		t.Fatal(err)
	}
	f.builds[b.Namespace+"/"+b.Name].Spec.BuildTarget.Arch = "other"
	if _, _, err := m.recoverOne(context.Background(), coordinationID(b.Namespace, "os", "arch")); err == nil {
		t.Fatal("identity mismatch ignored")
	}
	if _, err := m.get(context.Background(), coordinationID(b.Namespace, "os", "arch")); err != nil {
		t.Fatal("identity mismatch released claim")
	}
}

func TestClaimScanPagination(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	for i := 0; i < 101; i++ {
		b := testBuild(time.Unix(int64(i), 0).UTC().Format("150405"), "full")
		b.Spec.BuildTarget.Arch = b.Name
		if _, err := m.acquire(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.docs) != 0 {
		t.Fatalf("scan left %d Reserved claims", len(f.docs))
	}
}

func TestClaimLateBuildAndVersionedCleanup(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	b := testBuild("a", "full")
	if _, err := m.create(context.Background(), b, false, func() (runtime.Object, error) { return nil, errUnknown }); err != errUnknown {
		t.Fatal(err)
	}
	id := coordinationID(b.Namespace, "os", "arch")
	unknown, _, err := m.recoverOne(context.Background(), id)
	if err != nil || !unknown {
		t.Fatalf("unknown=%v err=%v", unknown, err)
	}
	// The original ES write arrives after the confirmation GET returned 404.
	if _, err := f.persist(b)(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.recoverOne(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	old, err := m.get(context.Background(), id)
	if err != nil || old.State != bound {
		t.Fatalf("late creation not recovered: %v", err)
	}
	f.builds[b.Namespace+"/"+b.Name].Status.Phase = ebsv1.BuildFailed
	if _, _, err := m.recoverOne(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	next := testBuild("b", "full")
	if _, err := m.create(context.Background(), next, false, f.persist(next)); err != nil {
		t.Fatal(err)
	}
	if err := m.release(context.Background(), old, "Terminal"); !es.IsStatus(err, 409) {
		t.Fatalf("stale release not fenced: %v", err)
	}
	c, err := m.get(context.Background(), id)
	if err != nil || c.BuildName != next.Name {
		t.Fatal("new owner lost")
	}
}

func TestClaimRecoveryFinishesReleasing(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	c, err := m.acquire(context.Background(), testBuild("a", "full"))
	if err != nil {
		t.Fatal(err)
	}
	f.transitionUnknown = releasing
	if err := m.release(context.Background(), c, "CreateNotSent"); err == nil {
		t.Fatal("expected response loss")
	}
	if err := m.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.get(context.Background(), c.id()); !es.IsStatus(err, 404) {
		t.Fatal("Releasing not recovered")
	}
}

func TestClaimDryRunOccupiedAndCanceledRecovery(t *testing.T) {
	f := newCoordinationFake()
	m := testManager(f)
	b := testBuild("a", "full")
	if _, err := m.create(context.Background(), b, false, f.persist(b)); err != nil {
		t.Fatal(err)
	}
	before := len(f.docs)
	if _, err := m.create(context.Background(), testBuild("b", "full"), true, func() (runtime.Object, error) { t.Fatal("dry run passed occupied target"); return nil, nil }); !apierrors.IsConflict(err) {
		t.Fatal(err)
	}
	if len(f.docs) != before {
		t.Fatal("dry run changed reservations")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.run(ctx)
	if len(f.docs) != before {
		t.Fatal("canceled recovery changed reservations")
	}
}
