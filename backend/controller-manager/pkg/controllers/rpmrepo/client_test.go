package rpmrepo

import (
	"context"
	"errors"
	"strconv"
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

// testAPIClient scripts responses and records requests made by the typed adapter.
type testAPIClient struct {
	clientpkg.Interface
	getResults  []runtime.Object
	getCalls    int
	listPages   []source.ListPage
	listErrors  []error
	listOptions []metav1.ListOptions
	listGVRs    []schema.GroupVersionResource
	updateObj   runtime.Object
	updateCalls int
}

func (s *testAPIClient) Get(context.Context, schema.GroupVersionResource, string, string) (runtime.Object, error) {
	index := s.getCalls
	s.getCalls++
	if index >= len(s.getResults) {
		return nil, errors.New("unexpected Get call")
	}
	return s.getResults[index], nil
}

func (s *testAPIClient) ListProjectPage(
	_ context.Context, gvr schema.GroupVersionResource, _ string, options metav1.ListOptions,
) (source.ListPage, error) {
	index := len(s.listOptions)
	s.listOptions = append(s.listOptions, options)
	s.listGVRs = append(s.listGVRs, gvr)
	if index < len(s.listErrors) && s.listErrors[index] != nil {
		return source.ListPage{}, s.listErrors[index]
	}
	if index >= len(s.listPages) {
		return source.ListPage{}, nil
	}
	return s.listPages[index], nil
}

func (s *testAPIClient) Update(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	s.updateCalls++
	return s.updateObj, nil
}

func (s *testAPIClient) UpdateStatus(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	s.updateCalls++
	return s.updateObj, nil
}

func jobObject(name string) runtime.Object {
	return &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testProject, UID: types.UID("uid-" + name), ResourceVersion: "1"}}
}

// endlessCursorClient keeps handing out fresh cursors so a test can exercise the page budget.
type endlessCursorClient struct {
	testAPIClient
	pages int
}

func (e *endlessCursorClient) ListProjectPage(context.Context, schema.GroupVersionResource, string, metav1.ListOptions) (source.ListPage, error) {
	e.pages++
	return source.ListPage{Items: []runtime.Object{jobObject("job-a")}, Continue: "cursor-" + strconv.Itoa(e.pages)}, nil
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
			shared := &testAPIClient{getResults: []runtime.Object{tc.response}}
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
	build := newBuild(testBuild)
	info := newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	shared := &testAPIClient{getResults: []runtime.Object{testRpmRepo(), build, info}}
	client := newAPIClient(shared)
	repo, err := client.GetRpmRepo(context.Background(), testProject, testBuild)
	if err != nil {
		t.Fatalf("GetRpmRepo: %v", err)
	}
	if repo.Name != testBuild || repo.Namespace != testProject {
		t.Fatalf("unexpected object %+v", repo.ObjectMeta)
	}
	gotBuild, err := client.GetBuild(context.Background(), testProject, testBuild)
	if err != nil || gotBuild.UID != build.UID {
		t.Fatalf("GetBuild: %+v %v", gotBuild, err)
	}
	gotInfo, err := client.GetBuildInfo(context.Background(), testProject, testBuild)
	if err != nil || gotInfo.UID != info.UID {
		t.Fatalf("GetBuildInfo: %+v %v", gotInfo, err)
	}
}

func TestAdapterUpdatesRpmRepoMetadata(t *testing.T) {
	response := testRpmRepo()
	response.Finalizers = []string{"other/controller"}
	shared := &testAPIClient{updateObj: response}
	updated, err := newAPIClient(shared).UpdateRpmRepo(context.Background(), response.DeepCopy())
	if err != nil || updated.UID != response.UID || shared.updateCalls != 1 {
		t.Fatalf("metadata update: %+v %v", updated, err)
	}
}

func TestAdapterClassifiesStatusWrites(t *testing.T) {
	t.Run("nil-request", func(t *testing.T) {
		_, err := newAPIClient(&testAPIClient{}).UpdateRpmRepoStatus(context.Background(), nil)
		var writeErr *clientpkg.WriteError
		if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteNotSent {
			t.Fatalf("a nil request must be a NotSent write error, got %v", err)
		}
	})

	t.Run("identity-mismatch", func(t *testing.T) {
		response := testRpmRepo()
		response.UID = "other-uid"
		shared := &testAPIClient{updateObj: response}
		_, err := newAPIClient(shared).UpdateRpmRepoStatus(context.Background(), testRpmRepo())
		var writeErr *clientpkg.WriteError
		if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteUnknown {
			t.Fatalf("an identity mismatch on the response must be WriteUnknown, got %v", err)
		}
	})

	t.Run("wrong-type", func(t *testing.T) {
		shared := &testAPIClient{updateObj: &ebsv1.Build{}}
		_, err := newAPIClient(shared).UpdateRpmRepoStatus(context.Background(), testRpmRepo())
		var writeErr *clientpkg.WriteError
		if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteUnknown {
			t.Fatalf("an unexpected response type must be WriteUnknown, got %v", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		shared := &testAPIClient{updateObj: testRpmRepo()}
		updated, err := newAPIClient(shared).UpdateRpmRepoStatus(context.Background(), testRpmRepo())
		if err != nil {
			t.Fatalf("UpdateRpmRepoStatus: %v", err)
		}
		if updated.Name != testBuild || shared.updateCalls != 1 {
			t.Fatalf("unexpected update result %+v", updated.ObjectMeta)
		}
	})
}

func TestAdapterRejectsUnexpectedListItems(t *testing.T) {
	shared := &testAPIClient{listPages: []source.ListPage{{Items: []runtime.Object{&ebsv1.Build{}}}}}
	if _, err := newAPIClient(shared).ListJobs(context.Background(), testProject, metav1.ListOptions{}); err == nil {
		t.Fatalf("a Job list must reject foreign item types")
	}
	shared = &testAPIClient{listPages: []source.ListPage{{Items: []runtime.Object{&ebsv1.Job{}}}}}
	if _, err := newAPIClient(shared).ListRpmRepos(context.Background(), testProject, metav1.ListOptions{}); err == nil {
		t.Fatalf("an RpmRepo list must reject foreign item types")
	}
}

func TestListReadsEveryPageAndKeepsSelectors(t *testing.T) {
	firstRepo := testRpmRepo()
	secondRepo := testRpmRepo()
	secondRepo.Name = "build-b"
	secondRepo.UID = "uid-build-b"
	tests := []struct {
		name    string
		gvr     schema.GroupVersionResource
		first   runtime.Object
		last    runtime.Object
		options metav1.ListOptions
		list    func(Client, metav1.ListOptions) ([]string, error)
		want    []string
	}{
		{
			name: "jobs", gvr: source.JobsGVR,
			first: jobObject("job-a"), last: jobObject("job-b"),
			options: metav1.ListOptions{LabelSelector: ebsv1.JobBuildNameLabel + "=" + testBuild},
			list: func(c Client, options metav1.ListOptions) ([]string, error) {
				items, err := c.ListJobs(context.Background(), testProject, options)
				names := make([]string, len(items))
				for i := range items {
					names[i] = items[i].Name
				}
				return names, err
			},
			want: []string{"job-a", "job-b"},
		},
		{
			name: "rpmrepos", gvr: source.RpmReposGVR,
			first: firstRepo, last: secondRepo,
			options: metav1.ListOptions{
				FieldSelector: nonTerminalRpmRepoFieldSelector,
				LabelSelector: ebsv1.BuildTargetArchLabel + "=" + testArch,
			},
			list: func(c Client, options metav1.ListOptions) ([]string, error) {
				items, err := c.ListRpmRepos(context.Background(), testProject, options)
				names := make([]string, len(items))
				for i := range items {
					names[i] = items[i].Name
				}
				return names, err
			},
			want: []string{testBuild, "build-b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shared := &testAPIClient{listPages: []source.ListPage{
				{Items: []runtime.Object{tt.first}, Continue: "cursor-1"},
				{Continue: "cursor-2"},
				{Items: []runtime.Object{tt.last}},
			}}
			names, err := tt.list(newAPIClient(shared), tt.options)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(names) != len(tt.want) {
				t.Fatalf("names = %v, want %v", names, tt.want)
			}
			for i := range names {
				if names[i] != tt.want[i] {
					t.Fatalf("names = %v, want %v", names, tt.want)
				}
			}
			cursors := []string{"", "cursor-1", "cursor-2"}
			if len(shared.listOptions) != len(cursors) {
				t.Fatalf("expected three page reads, got %d", len(shared.listOptions))
			}
			for i, request := range shared.listOptions {
				if request.Continue != cursors[i] {
					t.Fatalf("page %d cursor = %q, want %q", i, request.Continue, cursors[i])
				}
				if request.FieldSelector != tt.options.FieldSelector || request.LabelSelector != tt.options.LabelSelector {
					t.Fatalf("selectors changed between pages: %+v", request)
				}
				if request.Limit != defaultListPageSize {
					t.Fatalf("limit = %d, want %d", request.Limit, defaultListPageSize)
				}
				if shared.listGVRs[i] != tt.gvr {
					t.Fatalf("GVR = %v, want %v", shared.listGVRs[i], tt.gvr)
				}
			}
		})
	}
}

func TestListJobsDiscardsPartialResultsOnFailure(t *testing.T) {
	shared := &testAPIClient{
		listPages:  []source.ListPage{{Items: []runtime.Object{jobObject("job-a")}, Continue: "cursor-1"}},
		listErrors: []error{nil, errors.New("second page failed")},
	}
	client := newAPIClient(shared)

	items, err := client.ListJobs(context.Background(), testProject, metav1.ListOptions{})
	if err == nil {
		t.Fatalf("a failing page must fail the whole call")
	}
	if items != nil {
		t.Fatalf("partial results must be discarded, got %d items", len(items))
	}
}

func TestListJobsRejectsRepeatedCursor(t *testing.T) {
	shared := &testAPIClient{listPages: []source.ListPage{
		{Items: []runtime.Object{jobObject("job-a")}, Continue: "cursor-1"},
		{Items: []runtime.Object{jobObject("job-b")}, Continue: "cursor-1"},
	}}
	client := newAPIClient(shared)

	_, err := client.ListJobs(context.Background(), testProject, metav1.ListOptions{})
	if err == nil || !controller.IsPermanent(classifyReadError(err)) {
		t.Fatalf("a repeated cursor must be a permanent contract error, got %v", err)
	}
}

func TestListRpmReposRejectsNegativeLimit(t *testing.T) {
	client := newAPIClient(&testAPIClient{})
	if _, err := client.ListRpmRepos(context.Background(), testProject, metav1.ListOptions{Limit: -1}); err == nil {
		t.Fatalf("a negative limit must be rejected locally")
	}
}

func TestListAllStopsAtPageBudget(t *testing.T) {
	shared := &endlessCursorClient{}
	client := newAPIClient(shared)
	if _, err := client.ListJobs(context.Background(), testProject, metav1.ListOptions{}); err == nil {
		t.Fatalf("an endless cursor stream must be rejected")
	}
	if shared.pages != maxListPages {
		t.Fatalf("expected the read to stop after %d pages, got %d", maxListPages, shared.pages)
	}
}
