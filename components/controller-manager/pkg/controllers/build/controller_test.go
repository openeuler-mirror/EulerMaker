package build

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"controller-manager/pkg/manager"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
)

func TestNewValidatesDependenciesAndConfig(t *testing.T) {
	api := newFakeAPI()
	valid := Config{PollPeriod: time.Second, MaxRetries: 2}
	if _, err := New(nil, api, clock.RealClock{}, valid); err == nil {
		t.Fatal("a missing source must be rejected")
	}
	if _, err := New(&fakeSource{}, nil, clock.RealClock{}, valid); err == nil {
		t.Fatal("a missing client must be rejected")
	}
	if _, err := New(&fakeSource{}, api, nil, valid); err == nil {
		t.Fatal("a missing clock must be rejected")
	}
	for _, config := range []Config{{PollPeriod: 0, MaxRetries: 1}, {PollPeriod: time.Second, MaxRetries: -1}} {
		if _, err := New(&fakeSource{}, api, clock.RealClock{}, config); err == nil {
			t.Fatalf("invalid config %+v must fail startup", config)
		}
	}
}

func TestInitializerRegistersNonTerminalPollingSource(t *testing.T) {
	polling := &fakePollingFactory{}
	init := manager.InitContext{
		Dependencies: manager.Dependencies{Client: &stubSharedClient{}, PollingFactory: polling},
		Config:       manager.ControllerConfig{SlowRetryInitial: time.Second, SlowRetryMax: time.Minute, SlowRetryJitter: 0.2},
	}
	value, ok, err := Initializer(Config{PollPeriod: 25 * time.Second, MaxRetries: 3})(context.Background(), init)
	if err != nil || !ok || value == nil {
		t.Fatalf("value=%v ok=%v err=%v", value, ok, err)
	}
	if value.Name() != Name {
		t.Fatalf("controller name = %q", value.Name())
	}
	if polling.calls != 1 || polling.gvr != source.BuildsGVR {
		t.Fatalf("polling request = %+v", polling)
	}
	if polling.period != 25*time.Second {
		t.Fatalf("poll period = %s", polling.period)
	}
	if polling.options.FieldSelector != nonTerminalBuildFieldSelector {
		t.Fatalf("field selector = %q", polling.options.FieldSelector)
	}
	for _, phase := range []string{"Success", "Failed", "Aborted", "Skipped"} {
		if !strings.Contains(polling.options.FieldSelector, "status.phase!="+phase) {
			t.Fatalf("field selector %q does not exclude %s", polling.options.FieldSelector, phase)
		}
	}
	if strings.Contains(polling.options.FieldSelector, "metadata.") {
		t.Fatal("the polling filter must not depend on unsupported fields")
	}
}

func TestInitializerFailsFastOnInvalidConfig(t *testing.T) {
	polling := &fakePollingFactory{}
	init := manager.InitContext{
		Dependencies: manager.Dependencies{Client: &stubSharedClient{}, PollingFactory: polling},
		Config:       manager.ControllerConfig{SlowRetryInitial: time.Second, SlowRetryMax: time.Minute, SlowRetryJitter: 0.2},
	}
	value, ok, err := Initializer(Config{PollPeriod: 0, MaxRetries: 1})(context.Background(), init)
	if err == nil || ok || value != nil {
		t.Fatalf("value=%v ok=%v err=%v", value, ok, err)
	}
	if polling.calls != 0 {
		t.Fatal("an invalid config must fail before the polling source is requested")
	}
}

func TestEventHandlersEnqueueOnlyBuildAddsAndUpdates(t *testing.T) {
	api := newFakeAPI()
	build := newBuild("project-a", "build-a", "full", []string{"gcc"})
	c := newTestController(t, api, newTestClock())
	if c.Queue().Len() != 0 {
		t.Fatal("unexpected initial queue length")
	}
	c.onAdd(build)
	c.onUpdate(nil, build)
	if c.Queue().Len() != 1 {
		t.Fatalf("the periodic update must map to the same queue key: %d", c.Queue().Len())
	}
	key, shutdown := c.Queue().Get()
	if shutdown || key != "project-a/build-a" {
		t.Fatalf("key = %v shutdown=%v", key, shutdown)
	}
	c.Queue().Done(key)
	if c.Queue().Len() != 0 {
		t.Fatalf("queue length after drain = %d", c.Queue().Len())
	}

	c.onDelete(build)
	c.onAdd(&ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "project-a"}})
	c.onAdd(runtime.Object(nil))
	if c.Queue().Len() != 0 {
		t.Fatalf("delete and unexpected events must not enqueue: %d", c.Queue().Len())
	}
}

func TestStartupMarkerIsLoggedOnce(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	api.storeSnapshot(&ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotActive},
	})
	api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
	c := newTestController(t, api, newTestClock())

	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)

	// First round advances Pending to Prepared, second round advances Prepared to Processing.
	for round := 0; round < 2; round++ {
		if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if count := strings.Count(output.String(), "event=reconciled-after-start"); count != 1 {
		t.Fatalf("startup markers = %d in %q", count, output.String())
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildProcessing || stored.Status.Stage != stageBuild {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
}

func TestSplitKeyRejectsIncompleteKeys(t *testing.T) {
	for _, key := range []string{"", "build-a", "project-a/", "/build-a", "project-a/build-a/extra"} {
		if _, _, ok := splitKey(key); ok {
			t.Fatalf("key %q must be rejected", key)
		}
	}
	project, name, ok := splitKey("project-a/build-a")
	if !ok || project != "project-a" || name != "build-a" {
		t.Fatalf("project=%q name=%q ok=%v", project, name, ok)
	}
}
