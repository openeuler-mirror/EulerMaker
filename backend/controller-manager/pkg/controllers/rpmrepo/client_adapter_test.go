package rpmrepo

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// stubSharedClient implements the shared apiserver client surface so the typed adapter can be tested directly.
type stubSharedClient struct {
	getResults  []runtime.Object
	getErr      error
	getCalls    int
	listPages   []source.ListPage
	listErr     error
	listOptions []metav1.ListOptions
	listGVRs    []schema.GroupVersionResource
	updateObj   runtime.Object
	updateErr   error
	updateCalls int
}

func (s *stubSharedClient) ListPage(context.Context, schema.GroupVersionResource, metav1.ListOptions) (source.ListPage, error) {
	return source.ListPage{}, nil
}

func (s *stubSharedClient) ListProjectPage(_ context.Context, gvr schema.GroupVersionResource, _ string, options metav1.ListOptions) (source.ListPage, error) {
	s.listOptions = append(s.listOptions, options)
	s.listGVRs = append(s.listGVRs, gvr)
	index := len(s.listOptions) - 1
	if s.listErr != nil {
		return source.ListPage{}, s.listErr
	}
	if index >= len(s.listPages) {
		return source.ListPage{}, nil
	}
	return s.listPages[index], nil
}

func (s *stubSharedClient) ResolveWatch(context.Context, schema.GroupVersionResource) (source.WatchResource, error) {
	return source.WatchResource{}, nil
}

func (s *stubSharedClient) Get(context.Context, schema.GroupVersionResource, string, string) (runtime.Object, error) {
	index := s.getCalls
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	if index >= len(s.getResults) {
		return nil, errors.New("unexpected Get call")
	}
	return s.getResults[index], nil
}

func (s *stubSharedClient) Create(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	return nil, nil
}

func (s *stubSharedClient) Update(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	return nil, nil
}

func (s *stubSharedClient) UpdateStatus(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	s.updateCalls++
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	return s.updateObj, nil
}

func (s *stubSharedClient) Delete(context.Context, schema.GroupVersionResource, string, string, clientpkg.DeletePreconditions) error {
	return nil
}

func testRpmRepo() *ebsv1.RpmRepo {
	return &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Name: testBuild, Namespace: testProject, UID: types.UID("uid-" + testBuild), ResourceVersion: "1"},
	}
}

func TestAdapterRejectsUnexpectedGetResponses(t *testing.T) {
	cases := []struct {
		name     string
		response runtime.Object
		call     func(Client) error
	}{
		{
			name:     "build-name-mismatch",
			response: &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testProject, UID: "uid"}},
			call:     func(c Client) error { _, err := c.GetBuild(context.Background(), testProject, testBuild); return err },
		},
		{
			name:     "build-namespace-mismatch",
			response: &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: testBuild, Namespace: "other", UID: "uid"}},
			call:     func(c Client) error { _, err := c.GetBuild(context.Background(), testProject, testBuild); return err },
		},
		{
			name:     "build-without-uid",
			response: &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: testBuild, Namespace: testProject}},
			call:     func(c Client) error { _, err := c.GetBuild(context.Background(), testProject, testBuild); return err },
		},
		{
			name:     "buildinfo-wrong-type",
			response: &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: testBuild, Namespace: testProject, UID: "uid"}},
			call: func(c Client) error {
				_, err := c.GetBuildInfo(context.Background(), testProject, testBuild)
				return err
			},
		},
		{
			name:     "rpmrepo-wrong-type",
			response: &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: testBuild, Namespace: testProject, UID: "uid"}},
			call:     func(c Client) error { _, err := c.GetRpmRepo(context.Background(), testProject, testBuild); return err },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shared := &stubSharedClient{getResults: []runtime.Object{tc.response}}
			err := tc.call(newAPIClient(shared))
			if err == nil {
				t.Fatalf("an unexpected response must be rejected")
			}
			if !controller.IsPermanent(classifyReadError(err)) {
				t.Fatalf("a contract violation must classify as permanent, got %v", err)
			}
		})
	}
}

func TestAdapterAcceptsMatchingGetResponses(t *testing.T) {
	shared := &stubSharedClient{getResults: []runtime.Object{testRpmRepo()}}
	repo, err := newAPIClient(shared).GetRpmRepo(context.Background(), testProject, testBuild)
	if err != nil {
		t.Fatalf("GetRpmRepo: %v", err)
	}
	if repo.Name != testBuild || repo.Namespace != testProject {
		t.Fatalf("unexpected object %+v", repo.ObjectMeta)
	}
}

func TestAdapterClassifiesStatusWrites(t *testing.T) {
	t.Run("nil-request", func(t *testing.T) {
		_, err := newAPIClient(&stubSharedClient{}).UpdateRpmRepoStatus(context.Background(), nil)
		var writeErr *clientpkg.WriteError
		if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteNotSent {
			t.Fatalf("a nil request must be a NotSent write error, got %v", err)
		}
	})

	t.Run("identity-mismatch", func(t *testing.T) {
		response := testRpmRepo()
		response.UID = "other-uid"
		shared := &stubSharedClient{updateObj: response}
		_, err := newAPIClient(shared).UpdateRpmRepoStatus(context.Background(), testRpmRepo())
		var writeErr *clientpkg.WriteError
		if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteUnknown {
			t.Fatalf("an identity mismatch on the response must be WriteUnknown, got %v", err)
		}
	})

	t.Run("wrong-type", func(t *testing.T) {
		shared := &stubSharedClient{updateObj: &ebsv1.Build{}}
		_, err := newAPIClient(shared).UpdateRpmRepoStatus(context.Background(), testRpmRepo())
		var writeErr *clientpkg.WriteError
		if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteUnknown {
			t.Fatalf("an unexpected response type must be WriteUnknown, got %v", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		shared := &stubSharedClient{updateObj: testRpmRepo()}
		updated, err := newAPIClient(shared).UpdateRpmRepoStatus(context.Background(), testRpmRepo())
		if err != nil {
			t.Fatalf("UpdateRpmRepoStatus: %v", err)
		}
		if updated.Name != testBuild || shared.updateCalls != 1 {
			t.Fatalf("unexpected update result %+v", updated.ObjectMeta)
		}
	})
}

func TestAdapterAggregatesRpmRepoPagesAndKeepsSelectors(t *testing.T) {
	first := testRpmRepo()
	second := testRpmRepo()
	second.Name = "build-b"
	second.UID = "uid-build-b"
	shared := &stubSharedClient{listPages: []source.ListPage{
		{Items: []runtime.Object{first}, Continue: "cursor-1"},
		{Items: nil, Continue: "cursor-2"},
		{Items: []runtime.Object{second}},
	}}
	options := metav1.ListOptions{FieldSelector: nonTerminalRpmRepoFieldSelector, LabelSelector: "ebs.io/target-os=" + testOS}
	repos, err := newAPIClient(shared).ListRpmRepos(context.Background(), testProject, options)
	if err != nil {
		t.Fatalf("ListRpmRepos: %v", err)
	}
	if len(repos) != 2 || repos[1].Name != "build-b" {
		t.Fatalf("both pages must be aggregated, got %d items", len(repos))
	}
	if len(shared.listOptions) != 3 {
		t.Fatalf("expected three page reads, got %d", len(shared.listOptions))
	}
	if shared.listOptions[0].Continue != "" || shared.listOptions[1].Continue != "cursor-1" || shared.listOptions[2].Continue != "cursor-2" {
		t.Fatalf("continue cursors were not passed through: %+v", shared.listOptions)
	}
	for _, request := range shared.listOptions {
		if request.FieldSelector != options.FieldSelector || request.LabelSelector != options.LabelSelector {
			t.Fatalf("selectors must not change between pages: %+v", request)
		}
		if request.Limit != defaultListPageSize {
			t.Fatalf("an unset limit must default to %d, got %d", defaultListPageSize, request.Limit)
		}
	}
	for _, gvr := range shared.listGVRs {
		if gvr != source.RpmReposGVR {
			t.Fatalf("unexpected GVR %v", gvr)
		}
	}
}

func TestAdapterRejectsUnexpectedListItems(t *testing.T) {
	shared := &stubSharedClient{listPages: []source.ListPage{{Items: []runtime.Object{&ebsv1.Build{}}}}}
	if _, err := newAPIClient(shared).ListJobs(context.Background(), testProject, metav1.ListOptions{}); err == nil {
		t.Fatalf("a Job list must reject foreign item types")
	}
	shared = &stubSharedClient{listPages: []source.ListPage{{Items: []runtime.Object{&ebsv1.Job{}}}}}
	if _, err := newAPIClient(shared).ListRpmRepos(context.Background(), testProject, metav1.ListOptions{}); err == nil {
		t.Fatalf("an RpmRepo list must reject foreign item types")
	}
}
