package runner

import (
	"context"
	"fmt"
	"testing"
)

type fakeScriptSource struct {
	script ScriptResource
	calls  int
}

func (f *fakeScriptSource) GetScript(_ context.Context, name string) (*ScriptResource, error) {
	f.calls++
	if name != f.script.Metadata.Name {
		return nil, fmt.Errorf("Script %q not found", name)
	}
	script := f.script
	return &script, nil
}

func testScript(name, uid, resourceVersion, content string) ScriptResource {
	return ScriptResource{
		TypeMeta: TypeMeta{APIVersion: "ebs/v1", Kind: "Script"},
		Metadata: ObjectMeta{Name: name, UID: uid, ResourceVersion: resourceVersion},
		Spec:     ScriptSpec{Content: content},
	}
}

func TestScriptCacheRefreshesOnJobUIDOrResourceVersionChange(t *testing.T) {
	source := &fakeScriptSource{script: testScript("rpmbuild", "uid-1", "rv-1", "#!/bin/sh\necho first\n")}
	cache := NewScriptCache(source)
	ref := ScriptRef{Name: "rpmbuild", UID: "uid-1", ResourceVersion: "rv-1"}
	for i := 0; i < 2; i++ {
		content, err := cache.Resolve(context.Background(), ref)
		if err != nil || content != source.script.Spec.Content {
			t.Fatalf("resolve %d = %q, %v", i, content, err)
		}
	}
	if source.calls != 1 {
		t.Fatalf("GET calls = %d, want one cache miss", source.calls)
	}
	source.script = testScript("rpmbuild", "uid-1", "rv-2", "#!/bin/sh\necho second\n")
	ref.ResourceVersion = "rv-2"
	if content, err := cache.Resolve(context.Background(), ref); err != nil || content != source.script.Spec.Content || source.calls != 2 {
		t.Fatalf("resourceVersion refresh = %q, %v, calls=%d", content, err, source.calls)
	}
	source.script = testScript("rpmbuild", "uid-2", "rv-1", "#!/bin/sh\necho third\n")
	ref.UID, ref.ResourceVersion = "uid-2", "rv-1"
	if content, err := cache.Resolve(context.Background(), ref); err != nil || content != source.script.Spec.Content || source.calls != 3 {
		t.Fatalf("UID refresh = %q, %v, calls=%d", content, err, source.calls)
	}
}

func TestScriptCacheDoesNotCacheInvalidContent(t *testing.T) {
	source := &fakeScriptSource{script: testScript("rpmbuild", "uid-1", "rv-1", "echo unsafe\n")}
	cache := NewScriptCache(source)
	ref := ScriptRef{Name: "rpmbuild", UID: "uid-1", ResourceVersion: "rv-1"}
	if _, err := cache.Resolve(context.Background(), ref); err == nil {
		t.Fatal("invalid Script accepted")
	}
	source.script.Spec.Content = "#!/bin/sh\necho fixed\n"
	if _, err := cache.Resolve(context.Background(), ref); err != nil || source.calls != 2 {
		t.Fatalf("valid Script not refetched: %v, calls=%d", err, source.calls)
	}
}

func TestScriptCacheRefetchesWhenObservedVersionIsStale(t *testing.T) {
	source := &fakeScriptSource{script: testScript("rpmbuild", "uid-1", "rv-2", "#!/bin/sh\necho current\n")}
	cache := NewScriptCache(source)
	ref := ScriptRef{Name: "rpmbuild", UID: "uid-1", ResourceVersion: "rv-1"}
	for i := 0; i < 2; i++ {
		content, err := cache.Resolve(context.Background(), ref)
		if err != nil || content != source.script.Spec.Content {
			t.Fatalf("resolve %d = %q, %v", i, content, err)
		}
	}
	if source.calls != 2 {
		t.Fatalf("GET calls = %d, want a fresh read for each stale observation", source.calls)
	}
	ref.ResourceVersion = "rv-2"
	if _, err := cache.Resolve(context.Background(), ref); err != nil || source.calls != 2 {
		t.Fatalf("current observation did not hit cache: %v, calls=%d", err, source.calls)
	}
}
