package source

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
	list := func(_ context.Context, _ schema.GroupVersionResource, token string, _ int64) (ListPage, error) {
		return pages[token], nil
	}
	s, err := NewPollingSource(gvr, time.Second, 10, time.Second, list)
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
	s.list = func(_ context.Context, _ schema.GroupVersionResource, token string, _ int64) (ListPage, error) {
		if token == "broken" {
			return ListPage{}, errors.New("page failed")
		}
		return pages[token], nil
	}
	if err := s.scan(context.Background(), handlers); err == nil {
		t.Fatal("expected page failure")
	}
	if len(s.snapshot) != 2 {
		t.Fatalf("failed scan replaced snapshot: %d", len(s.snapshot))
	}
}

func TestPollingSourceRegistrationFreezesAtRun(t *testing.T) {
	s, err := NewPollingSource(schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "builds"}, time.Hour, 10, time.Hour, func(context.Context, schema.GroupVersionResource, string, int64) (ListPage, error) {
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
	f := NewPollingSourceFactory(func(context.Context, schema.GroupVersionResource, string, int64) (ListPage, error) {
		return ListPage{}, nil
	}, 10, time.Minute)
	gvr := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "builds"}
	if _, err := f.ForResource(gvr, time.Minute); err != nil {
		t.Fatal(err)
	}
	_ = f.Sources()
	if _, err := f.ForResource(gvr, time.Second); !errors.Is(err, ErrSourceStarted) {
		t.Fatalf("expected frozen factory, got %v", err)
	}
}
