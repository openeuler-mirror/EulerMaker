package build

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type getCall struct {
	gvr             schema.GroupVersionResource
	namespace, name string
}

type listCall struct {
	gvr     schema.GroupVersionResource
	project string
	options metav1.ListOptions
}

type stubSharedClient struct {
	mu           sync.Mutex
	gets         []getCall
	lists        []listCall
	getResult    runtime.Object
	getErr       error
	listResult   source.ListPage
	listErr      error
	createResult runtime.Object
	createErr    error
	updateResult runtime.Object
	updateErr    error
}

func (s *stubSharedClient) ListPage(context.Context, schema.GroupVersionResource, metav1.ListOptions) (source.ListPage, error) {
	return source.ListPage{}, nil
}

func (s *stubSharedClient) ListProjectPage(_ context.Context, gvr schema.GroupVersionResource, project string, options metav1.ListOptions) (source.ListPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists = append(s.lists, listCall{gvr: gvr, project: project, options: options})
	return s.listResult, s.listErr
}

func (s *stubSharedClient) ResolveWatch(context.Context, schema.GroupVersionResource) (source.WatchResource, error) {
	return source.WatchResource{}, nil
}

func (s *stubSharedClient) Get(_ context.Context, gvr schema.GroupVersionResource, namespace, name string) (runtime.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets = append(s.gets, getCall{gvr: gvr, namespace: namespace, name: name})
	return s.getResult, s.getErr
}

func (s *stubSharedClient) Create(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	return s.createResult, s.createErr
}

func (s *stubSharedClient) Update(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	return s.updateResult, s.updateErr
}

func (s *stubSharedClient) UpdateStatus(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	return s.updateResult, s.updateErr
}

func (s *stubSharedClient) Delete(context.Context, schema.GroupVersionResource, string, string, clientpkg.DeletePreconditions) error {
	return nil
}

func (s *stubSharedClient) lastGet(t *testing.T) getCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.gets) == 0 {
		t.Fatal("no Get call was made")
	}
	return s.gets[len(s.gets)-1]
}

func TestGetLastPublishedBuildScopesTheQueryToTheProject(t *testing.T) {
	previous := withPhaseStage(newBuild("project-a", "build-prev", "full", []string{"gcc"}), ebsv1.BuildSuccess, stagePublish)
	shared := &stubSharedClient{listResult: source.ListPage{Items: []runtime.Object{previous}}}
	client := newAPIClient(shared)
	build, err := client.GetLastPublishedBuild(context.Background(), "project-a", "openEuler-22.03-LTS", "aarch64")
	if err != nil || build == nil || build.Name != "build-prev" {
		t.Fatalf("build=%+v err=%v", build, err)
	}
	if len(shared.lists) != 1 {
		t.Fatalf("list calls = %d", len(shared.lists))
	}
	call := shared.lists[0]
	if call.gvr != source.BuildsGVR || call.project != "project-a" {
		t.Fatalf("list call = %+v", call)
	}
	if call.options.Limit != 1 {
		t.Fatalf("limit = %d", call.options.Limit)
	}
	if call.options.FieldSelector != lastPublishedBuildFieldSelector {
		t.Fatalf("field selector = %q", call.options.FieldSelector)
	}
	for _, label := range []string{ebsv1.BuildTargetOSLabel + "=openEuler-22.03-LTS", ebsv1.BuildTargetArchLabel + "=aarch64"} {
		if !strings.Contains(call.options.LabelSelector, label) {
			t.Fatalf("label selector %q misses %q", call.options.LabelSelector, label)
		}
	}
}

func TestGetLastPublishedBuildWithoutHistory(t *testing.T) {
	shared := &stubSharedClient{}
	client := newAPIClient(shared)
	build, err := client.GetLastPublishedBuild(context.Background(), "project-a", "os", "arch")
	if err != nil || build != nil {
		t.Fatalf("build=%v err=%v", build, err)
	}
}

func TestTypedGettersValidateResponses(t *testing.T) {
	t.Run("project scope and identity", func(t *testing.T) {
		shared := &stubSharedClient{getResult: &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: "uid", ResourceVersion: "1"}}}
		client := newAPIClient(shared)
		_, err := client.GetProject(context.Background(), "project-a")
		assertContractError(t, err)
		if call := shared.lastGet(t); call.namespace != "" || call.gvr != source.ProjectsGVR {
			t.Fatalf("Get call = %+v", call)
		}
	})

	for _, tc := range []struct {
		name   string
		result runtime.Object
		read   func(Client) error
	}{
		{
			name:   "project wrong identity",
			result: &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: "uid", ResourceVersion: "1"}},
			read:   func(c Client) error { _, err := c.GetProject(context.Background(), "project-a"); return err },
		},
		{
			name:   "build wrong type",
			result: &ebsv1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", UID: "uid", ResourceVersion: "1"}},
			read:   func(c Client) error { _, err := c.GetBuild(context.Background(), "project-a", "build-a"); return err },
		},
		{
			name:   "build missing server metadata",
			result: &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"}},
			read:   func(c Client) error { _, err := c.GetBuild(context.Background(), "project-a", "build-a"); return err },
		},
		{
			name:   "snapshot wrong type",
			result: &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", UID: "uid", ResourceVersion: "1"}},
			read: func(c Client) error {
				_, err := c.GetSnapshot(context.Background(), "project-a", "build-a")
				return err
			},
		},
		{
			name:   "rpm repo wrong identity",
			result: &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "project-a", UID: "uid", ResourceVersion: "1"}},
			read:   func(c Client) error { _, err := c.GetRpmRepo(context.Background(), "project-a", "build-a"); return err },
		},
		{
			name:   "build info missing server metadata",
			result: &ebsv1.BuildInfo{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"}},
			read: func(c Client) error {
				_, err := c.GetBuildInfo(context.Background(), "project-a", "build-a")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newAPIClient(&stubSharedClient{getResult: tc.result})
			assertContractError(t, tc.read(client))
		})
	}

	t.Run("last published build missing server metadata", func(t *testing.T) {
		shared := &stubSharedClient{listResult: source.ListPage{Items: []runtime.Object{
			&ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: "build-prev", Namespace: "project-a"}},
		}}}
		client := newAPIClient(shared)
		_, err := client.GetLastPublishedBuild(context.Background(), "project-a", "openEuler-22.03-LTS", "aarch64")
		assertContractError(t, err)
	})

	t.Run("empty project input", func(t *testing.T) {
		client := newAPIClient(&stubSharedClient{})
		_, err := client.GetProject(context.Background(), "")
		assertContractError(t, err)
	})
}

// assertContractError checks that a read rejected a request or response the API contract does not allow, and
// that the read classification turns it into a permanent error instead of a retry.
func assertContractError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("a contract violation must be rejected")
	}
	var contract contractError
	if !errors.As(err, &contract) {
		t.Fatalf("want a contract error, got %T: %v", err, err)
	}
	if !controller.IsPermanent(classifyReadError(err)) {
		t.Fatalf("a contract violation must classify as permanent: %v", err)
	}
}

func TestClientKeepsWriteErrorClassification(t *testing.T) {
	rejected := &clientpkg.WriteError{Operation: "create", Outcome: clientpkg.WriteRejected, StatusCode: 409, Err: apierrors.NewAlreadyExists(snapshotsResource, "build-a")}
	shared := &stubSharedClient{createErr: rejected}
	client := newAPIClient(shared)
	_, err := client.CreateSnapshot(context.Background(), "project-a", &ebsv1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"}})
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteRejected || writeErr.StatusCode != 409 {
		t.Fatalf("write error = %v", err)
	}
}

func TestUpdateBuildStatusUnknownResponse(t *testing.T) {
	request := withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	shared := &stubSharedClient{updateResult: &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", UID: "other-uid", ResourceVersion: "1"}}}
	client := newAPIClient(shared)
	_, err := client.UpdateBuildStatus(context.Background(), request)
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteUnknown {
		t.Fatalf("write error = %v", err)
	}
}

func TestUpdateBuildChecksResponseIdentity(t *testing.T) {
	request := newBuild("project-a", "build-a", "incremental", []string{"gcc"})
	shared := &stubSharedClient{updateResult: request.DeepCopy()}
	updated, err := newAPIClient(shared).UpdateBuild(context.Background(), request)
	if err != nil || updated.Name != request.Name {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	shared.updateResult = &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: request.Name, Namespace: request.Namespace, UID: "other", ResourceVersion: "2"}}
	_, err = newAPIClient(shared).UpdateBuild(context.Background(), request)
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteUnknown {
		t.Fatalf("unexpected update response error = %v", err)
	}
}
