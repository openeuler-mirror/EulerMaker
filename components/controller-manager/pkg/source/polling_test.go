package source

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

type recordingHandler struct {
	mu     sync.Mutex
	events []string
}

func (h *recordingHandler) add(value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, value)
}
func (h *recordingHandler) OnAdd(obj runtime.Object) {
	h.add("add:" + obj.(*metav1.PartialObjectMetadata).Name)
}
func (h *recordingHandler) OnUpdate(_, obj runtime.Object) {
	h.add("update:" + obj.(*metav1.PartialObjectMetadata).Name)
}
func (h *recordingHandler) OnDelete(obj runtime.Object) {
	h.add("delete:" + obj.(*metav1.PartialObjectMetadata).Name)
}
func object(name, uid, rv string) runtime.Object {
	return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), ResourceVersion: rv}}
}

func TestPollingSourceScanAndFailedPageKeepsSnapshot(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "builds"}
	pages := map[string]ListPage{"": {Items: []runtime.Object{object("a", "1", "1")}, Continue: "next"}, "next": {Items: []runtime.Object{object("b", "2", "1")}}}
	list := func(_ context.Context, _ schema.GroupVersionResource, options metav1.ListOptions) (ListPage, error) {
		return pages[options.Continue], nil
	}
	s, err := NewPollingSource(gvr, time.Second, 10, time.Second, metav1.ListOptions{}, list)
	if err != nil {
		t.Fatal(err)
	}
	h := &recordingHandler{}
	if err := s.AddEventHandler(h); err != nil {
		t.Fatal(err)
	}
	handlers, err := s.subscriptions.start()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.scan(context.Background(), handlers); err != nil {
		t.Fatal(err)
	}
	if len(s.snapshot) != 2 || !s.HasSynced() || !s.Ready() {
		t.Fatalf("unexpected initial state: items=%d synced=%v ready=%v", len(s.snapshot), s.HasSynced(), s.Ready())
	}
	pages[""] = ListPage{Items: []runtime.Object{object("a", "1", "2")}, Continue: "broken"}
	s.list = func(_ context.Context, _ schema.GroupVersionResource, options metav1.ListOptions) (ListPage, error) {
		if options.Continue == "broken" {
			return ListPage{}, errors.New("page failed")
		}
		return pages[options.Continue], nil
	}
	if err := s.scan(context.Background(), handlers); err == nil {
		t.Fatal("expected page failure")
	}
	if len(s.snapshot) != 2 {
		t.Fatalf("failed scan replaced snapshot: %d", len(s.snapshot))
	}
}

func TestPollingSourceRegistrationFreezesAtRun(t *testing.T) {
	s, err := NewPollingSource(schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "builds"}, time.Hour, 10, time.Hour, metav1.ListOptions{}, func(context.Context, schema.GroupVersionResource, metav1.ListOptions) (ListPage, error) {
		return ListPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.AddEventHandler(&recordingHandler{}), ErrSourceStarted) {
		t.Fatal("registration after Run must fail")
	}
	if !errors.Is(s.Run(ctx), ErrSourceStarted) {
		t.Fatal("second Run must fail")
	}
}

func TestWatchFactoryRejectsUnsupportedResource(t *testing.T) {
	f := NewWatchSourceFactory(nil, 0, time.Minute)
	_, err := f.ForResource(schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "builds"})
	if !errors.Is(err, ErrWatchUnsupported) {
		t.Fatalf("expected ErrWatchUnsupported, got %v", err)
	}
}

func TestPollingFactoryFreezesAfterSources(t *testing.T) {
	f := NewPollingSourceFactory(func(context.Context, schema.GroupVersionResource, metav1.ListOptions) (ListPage, error) {
		return ListPage{}, nil
	}, 10, time.Minute)
	gvr := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "builds"}
	if _, err := f.ForResource(gvr, time.Minute, metav1.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = f.Sources()
	if _, err := f.ForResource(gvr, time.Second, metav1.ListOptions{}); !errors.Is(err, ErrSourceStarted) {
		t.Fatalf("expected frozen factory, got %v", err)
	}
}

func TestPollingSourcePassesSelectorsToEveryPage(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "builds"}
	want := metav1.ListOptions{LabelSelector: "build.ebs.io/os=openEuler", FieldSelector: "status.phase=Pending"}
	var calls []metav1.ListOptions
	list := func(_ context.Context, _ schema.GroupVersionResource, options metav1.ListOptions) (ListPage, error) {
		calls = append(calls, options)
		if options.Continue == "" {
			return ListPage{Continue: "next"}, nil
		}
		return ListPage{}, nil
	}
	s, err := NewPollingSource(gvr, time.Second, 25, time.Minute, want, list)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.scan(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d List calls, want 2", len(calls))
	}
	for i, options := range calls {
		if options.LabelSelector != want.LabelSelector || options.FieldSelector != want.FieldSelector || options.Limit != 25 {
			t.Fatalf("call %d options = %#v", i, options)
		}
	}
	if calls[0].Continue != "" || calls[1].Continue != "next" {
		t.Fatalf("unexpected continue tokens: %#v", calls)
	}
}

func TestPollingFactorySharesOnlyIdenticallyFilteredSources(t *testing.T) {
	f := NewPollingSourceFactory(func(context.Context, schema.GroupVersionResource, metav1.ListOptions) (ListPage, error) {
		return ListPage{}, nil
	}, 10, time.Minute)
	gvr := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "builds"}
	first, err := f.ForResource(gvr, time.Minute, metav1.ListOptions{LabelSelector: "team=a", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	same, err := f.ForResource(gvr, time.Second, metav1.ListOptions{LabelSelector: "team=a", Continue: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if first != same {
		t.Fatal("identical selectors did not share a source")
	}
	different, err := f.ForResource(gvr, time.Minute, metav1.ListOptions{LabelSelector: "team=b"})
	if err != nil {
		t.Fatal(err)
	}
	if first == different {
		t.Fatal("different selectors shared a source")
	}
	if got := len(f.Sources()); got != 2 {
		t.Fatalf("got %d sources, want 2", got)
	}
}

func TestWatchSourceCacheReturnsDeepCopies(t *testing.T) {
	fakeWatch := watch.NewRaceFreeFake()
	lw := &cache.ListWatch{
		ListFunc:  func(metav1.ListOptions) (runtime.Object, error) { return &ebsv1.JobList{}, nil },
		WatchFunc: func(metav1.ListOptions) (watch.Interface, error) { return fakeWatch, nil },
	}
	s, err := NewWatchSource("jobs", lw, &ebsv1.Job{}, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetByKey("project/job"); !errors.Is(err, ErrCacheNotSynced) {
		t.Fatalf("GetByKey before sync returned %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitFor(t, time.Second, s.HasSynced, "watch source did not sync")

	fakeWatch.Add(&ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "project", UID: "uid", ResourceVersion: "1"}})
	waitFor(t, time.Second, func() bool {
		_, exists, err := s.GetByKey("project/job")
		return err == nil && exists
	}, "watch event did not reach cache")

	obj, exists, err := s.GetByKey("project/job")
	if err != nil || !exists {
		t.Fatalf("GetByKey failed: exists=%t err=%v", exists, err)
	}
	obj.(*ebsv1.Job).Name = "changed"
	again, _, err := s.GetByKey("project/job")
	if err != nil || again.(*ebsv1.Job).Name != "job" {
		t.Fatal("GetByKey exposed the cached object by reference")
	}
	indexed, err := s.ByIndex(cache.NamespaceIndex, "project")
	if err != nil || len(indexed) != 1 {
		t.Fatalf("ByIndex returned %d objects: %v", len(indexed), err)
	}
	if _, err := s.ByIndex("missing", "value"); !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("missing index returned %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}
