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

// pagingSharedClient returns one scripted page per call and records the options it received.
type pagingSharedClient struct {
	pages    []source.ListPage
	errs     []error
	requests []metav1.ListOptions
}

func (p *pagingSharedClient) ListPage(context.Context, schema.GroupVersionResource, metav1.ListOptions) (source.ListPage, error) {
	return source.ListPage{}, nil
}

func (p *pagingSharedClient) ListProjectPage(_ context.Context, _ schema.GroupVersionResource, _ string, options metav1.ListOptions) (source.ListPage, error) {
	index := len(p.requests)
	p.requests = append(p.requests, options)
	if index < len(p.errs) && p.errs[index] != nil {
		return source.ListPage{}, p.errs[index]
	}
	if index >= len(p.pages) {
		return source.ListPage{}, nil
	}
	return p.pages[index], nil
}

func (p *pagingSharedClient) ResolveWatch(context.Context, schema.GroupVersionResource) (source.WatchResource, error) {
	return source.WatchResource{}, nil
}

func (p *pagingSharedClient) Get(context.Context, schema.GroupVersionResource, string, string) (runtime.Object, error) {
	return nil, nil
}

func (p *pagingSharedClient) Create(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	return nil, nil
}

func (p *pagingSharedClient) Update(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	return nil, nil
}

func (p *pagingSharedClient) UpdateStatus(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error) {
	return nil, nil
}

func (p *pagingSharedClient) Delete(context.Context, schema.GroupVersionResource, string, string, clientpkg.DeletePreconditions) error {
	return nil
}

func jobObject(name string) runtime.Object {
	return &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testProject, UID: types.UID("uid-" + name), ResourceVersion: "1"}}
}

// endlessCursorClient keeps handing out fresh cursors so a test can exercise the page budget.
type endlessCursorClient struct {
	pagingSharedClient
	pages int
}

func (e *endlessCursorClient) ListProjectPage(context.Context, schema.GroupVersionResource, string, metav1.ListOptions) (source.ListPage, error) {
	e.pages++
	return source.ListPage{Items: []runtime.Object{jobObject("job-a")}, Continue: "cursor-" + strconv.Itoa(e.pages)}, nil
}

func TestListJobsReadsEveryPage(t *testing.T) {
	shared := &pagingSharedClient{pages: []source.ListPage{
		{Items: []runtime.Object{jobObject("job-a")}, Continue: "cursor-1"},
		{Items: nil, Continue: "cursor-2"},
		{Items: []runtime.Object{jobObject("job-b")}},
	}}
	client := newAPIClient(shared)

	items, err := client.ListJobs(context.Background(), testProject, metav1.ListOptions{LabelSelector: "ebs.io/build-name=" + testBuild})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected both pages to be aggregated, got %d items", len(items))
	}
	if len(shared.requests) != 3 {
		t.Fatalf("expected three page requests, got %d", len(shared.requests))
	}
	if shared.requests[0].Continue != "" || shared.requests[1].Continue != "cursor-1" || shared.requests[2].Continue != "cursor-2" {
		t.Fatalf("continue cursors were not passed through: %+v", shared.requests)
	}
	for _, request := range shared.requests {
		if request.LabelSelector != "ebs.io/build-name="+testBuild {
			t.Fatalf("the query conditions must not change between pages: %+v", request)
		}
		if request.Limit != defaultListPageSize {
			t.Fatalf("an unset limit must default to %d, got %d", defaultListPageSize, request.Limit)
		}
	}
}

func TestListJobsDiscardsPartialResultsOnFailure(t *testing.T) {
	shared := &pagingSharedClient{
		pages: []source.ListPage{{Items: []runtime.Object{jobObject("job-a")}, Continue: "cursor-1"}},
		errs:  []error{nil, errors.New("second page failed")},
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
	shared := &pagingSharedClient{pages: []source.ListPage{
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
	client := newAPIClient(&pagingSharedClient{})
	if _, err := client.ListRpmRepos(context.Background(), testProject, metav1.ListOptions{Limit: -1}); err == nil {
		t.Fatalf("a negative limit must be rejected locally")
	}
}
