package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestResourceObjectsAndListPages(t *testing.T) {
	client, err := New(testRESTConfig(nil), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range ebsv1.ResourceRegistrations() {
		t.Run(resource.Resource, func(t *testing.T) {
			gvr := ebsv1.SchemeGroupVersion.WithResource(resource.Resource)
			obj, err := client.newObject(gvr)
			if err != nil {
				t.Fatal(err)
			}
			list, err := client.newList(gvr)
			if err != nil {
				t.Fatal(err)
			}
			if reflect.TypeOf(obj) != reflect.TypeOf(resource.Object) || reflect.TypeOf(list) != reflect.TypeOf(resource.List) {
				t.Fatalf("object=%T list=%T, want %T and %T", obj, list, resource.Object, resource.List)
			}
			meta, err := apiMeta.Accessor(obj)
			if err != nil {
				t.Fatal(err)
			}
			meta.SetName("item")
			meta.SetLabels(map[string]string{"key": "original"})
			if err := apiMeta.SetList(list, []runtime.Object{obj}); err != nil {
				t.Fatal(err)
			}
			listMeta, err := apiMeta.ListAccessor(list)
			if err != nil {
				t.Fatal(err)
			}
			listMeta.SetContinue("next")
			listMeta.SetResourceVersion("123")
			page, err := listPage(list)
			if err != nil || len(page.Items) != 1 || page.Continue != "next" || page.ResourceVersion != "123" {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			itemMeta, err := apiMeta.Accessor(page.Items[0])
			if err != nil {
				t.Fatal(err)
			}
			if itemMeta.GetName() != "item" || itemMeta.GetLabels()["key"] != "original" {
				t.Fatalf("unexpected item: %+v", page.Items[0])
			}
			itemMeta.GetLabels()["key"] = "changed"
			unchanged, err := listPage(list)
			if err != nil {
				t.Fatal(err)
			}
			originalMeta, err := apiMeta.Accessor(unchanged.Items[0])
			if err != nil || originalMeta.GetLabels()["key"] != "original" {
				t.Fatalf("list item shares mutable labels with page: %v", err)
			}
			if err := apiMeta.SetList(list, nil); err != nil {
				t.Fatal(err)
			}
			empty, err := listPage(list)
			if err != nil || len(empty.Items) != 0 || empty.Continue != "next" || empty.ResourceVersion != "123" {
				t.Fatalf("empty page=%+v err=%v", empty, err)
			}
		})
	}
}

func TestListScripts(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/ebs/v1/scripts" || r.URL.Query().Get("continue") != "cursor" {
			t.Fatalf("request=%s %s", r.Method, r.URL)
		}
		return jsonResponse(r, []byte(`{"apiVersion":"ebs/v1","kind":"ScriptList","metadata":{"continue":"next","resourceVersion":"123"},"items":[{"metadata":{"name":"rpmbuild"},"spec":{"content":"echo build"}}]}`)), nil
	})
	client, err := New(testRESTConfig(transport), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.ListPage(context.Background(), source.ScriptsGVR, metav1.ListOptions{Continue: "cursor"})
	if err != nil || len(page.Items) != 1 || page.Continue != "next" || page.ResourceVersion != "123" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	script, ok := page.Items[0].(*ebsv1.Script)
	if !ok || script.Name != "rpmbuild" || script.Spec.Content != "echo build" {
		t.Fatalf("script=%+v", page.Items[0])
	}
}

func TestArchiveJobAndReadSnapshot(t *testing.T) {
	job := &ebsv1.Job{TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Job"}, ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "project", UID: "uid", ResourceVersion: "123"}}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			if r.URL.Path != "/apis/ebs/v1/projects/project/jobs/job/archive" {
				t.Fatalf("path=%s", r.URL.Path)
			}
			var input struct {
				Preconditions metav1.Preconditions `json:"preconditions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Preconditions.UID == nil || *input.Preconditions.UID != "uid" || input.Preconditions.ResourceVersion == nil || *input.Preconditions.ResourceVersion != "123" {
				t.Fatalf("input=%+v", input)
			}
		} else if r.Method != http.MethodGet || r.URL.Path != "/apis/ebs/v1/projects/project/oldjobs/job" || r.URL.RawQuery != "" {
			t.Fatalf("request=%s %s", r.Method, r.URL)
		}
		body, _ := json.Marshal(job)
		return jsonResponse(r, body), nil
	})
	client, err := New(testRESTConfig(transport), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	out, err := client.ArchiveJob(context.Background(), "project", "job", DeletePreconditions{UID: "uid", ResourceVersion: "123"})
	if err != nil || out.ResourceVersion != "123" {
		t.Fatalf("archive=%+v err=%v", out, err)
	}
	obj, err := client.Get(context.Background(), ebsv1.SchemeGroupVersion.WithResource("oldjobs"), "project", "job")
	if err != nil || obj.(*ebsv1.Job).ResourceVersion != "123" {
		t.Fatalf("GET=%+v err=%v", obj, err)
	}
}

func TestPostSubresource(t *testing.T) {
	for _, phase := range []ebsv1.JobPhase{ebsv1.JobAborted, ebsv1.JobSucceeded, ebsv1.JobRunning} {
		t.Run(string(phase), func(t *testing.T) {
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.Path != "/apis/ebs/v1/projects/project/jobs/job/abort" {
					t.Fatalf("unexpected abort request: %s %s", r.Method, r.URL.Path)
				}
				var input map[string]string
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Fatal(err)
				}
				if input["uid"] != "uid" || input["reason"] != "parent Build aborted" {
					t.Fatalf("abort body = %v", input)
				}
				body, err := json.Marshal(&ebsv1.Job{TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Job"}, ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "project", UID: "uid", ResourceVersion: "2"}, Status: ebsv1.JobStatus{Phase: phase}})
				if err != nil {
					t.Fatal(err)
				}
				return jsonResponse(r, body), nil
			})
			client, err := New(testRESTConfig(transport), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			obj, err := client.PostSubresource(
				context.Background(), source.JobsGVR, "project", "job", "abort",
				map[string]string{"uid": "uid", "reason": "parent Build aborted"},
			)
			if err != nil {
				t.Fatal(err)
			}
			job, ok := obj.(*ebsv1.Job)
			if !ok {
				t.Fatalf("unexpected response type: %T", obj)
			}
			if job.Status.Phase != phase {
				t.Fatalf("phase = %s, want %s", job.Status.Phase, phase)
			}
		})
	}
}

func TestPostSubresourceWriteOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		subresource string
		body        any
		status      int
		err         error
		outcome     WriteOutcome
	}{
		{name: "invalid path", subresource: "../abort", outcome: WriteNotSent},
		{name: "encoding failure", subresource: "abort", body: make(chan int), outcome: WriteNotSent},
		{name: "rejected", subresource: "abort", status: http.StatusForbidden, outcome: WriteRejected},
		{name: "transport failure", subresource: "abort", err: io.ErrUnexpectedEOF, outcome: WriteUnknown},
		{name: "invalid response", subresource: "abort", status: http.StatusOK, outcome: WriteUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client, err := New(testRESTConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.err != nil {
					return nil, tc.err
				}
				response := jsonResponse(r, []byte(`{}`))
				response.StatusCode = tc.status
				return response, nil
			})), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.PostSubresource(context.Background(), source.JobsGVR, "project", "job", tc.subresource, tc.body)
			var writeErr *WriteError
			if !errors.As(err, &writeErr) || writeErr.Outcome != tc.outcome {
				t.Fatalf("error = %v, want %s", err, tc.outcome)
			}
			if tc.outcome == WriteNotSent && calls != 0 {
				t.Fatalf("local failure sent %d requests", calls)
			}
			if tc.outcome == WriteRejected && writeErr.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", writeErr.StatusCode, tc.status)
			}
		})
	}
}

func TestClassifyWriteStatusError(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "jobs"}
	statusErr := apierrors.NewTooManyRequests("busy", 7)
	err := classifyWrite("update-status", gvr, statusErr)
	if err.Outcome != WriteRejected || err.StatusCode != 429 || err.RetryAfter != 7*time.Second {
		t.Fatalf("unexpected classification: %#v", err)
	}
	if !errors.Is(err, statusErr) {
		t.Fatal("WriteError does not unwrap the original status error")
	}
	if got := RetryAfter(fmt.Errorf("wrapped: %w", err)); got != 7*time.Second {
		t.Fatalf("RetryAfter=%s, want 7s", got)
	}
}

func TestClassifyWriteUnknown(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "jobs"}
	cause := errors.New("connection reset")
	err := classifyWrite("delete", gvr, cause)
	if err.Outcome != WriteUnknown || err.StatusCode != 0 || !errors.Is(err, cause) {
		t.Fatalf("unexpected classification: %#v", err)
	}
}

func TestRetryAfterRejectsUnsupportedOutcome(t *testing.T) {
	for _, err := range []*WriteError{
		{Outcome: WriteUnknown, StatusCode: 429, RetryAfter: time.Second},
		{Outcome: WriteRejected, StatusCode: 500, RetryAfter: time.Second},
		{Outcome: WriteRejected, StatusCode: 503},
	} {
		if got := RetryAfter(err); got != 0 {
			t.Fatalf("RetryAfter(%#v)=%s, want 0", err, got)
		}
	}
}

func TestValidateTargetScope(t *testing.T) {
	client, err := New(testRESTConfig(nil), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	jobs := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "jobs"}
	runners := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "runners"}
	scripts := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "scripts"}
	if err := client.validateTarget(jobs, "", "job"); err == nil {
		t.Fatal("namespace-scoped target accepted an empty namespace")
	}
	if err := client.validateTarget(runners, "project", "runner"); err == nil {
		t.Fatal("cluster-scoped target accepted a namespace")
	}
	if err := client.validateTarget(scripts, "project", "rpmbuild"); err == nil {
		t.Fatal("cluster-scoped Script accepted a namespace")
	}
	if err := client.validateTarget(scripts, "", "rpmbuild"); err != nil {
		t.Fatal(err)
	}
	if err := client.validateTarget(jobs, "project", "job"); err != nil {
		t.Fatal(err)
	}
}

func TestGetClusterScopedScript(t *testing.T) {
	scripts := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "scripts"}
	response := &ebsv1.Script{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Script"},
		ObjectMeta: metav1.ObjectMeta{Name: "rpmbuild", UID: "61304b92-72cf-4a41-8bf7-8e0a9d14f6a5", ResourceVersion: "v1:1:1"},
		Spec:       ebsv1.ScriptSpec{Content: "#!/bin/sh\n"},
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/ebs/v1/scripts/rpmbuild" {
			return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		return jsonResponse(r, body), nil
	})
	client, err := New(testRESTConfig(transport), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Get(context.Background(), scripts, "", "rpmbuild")
	if err != nil {
		t.Fatal(err)
	}
	if got.(*ebsv1.Script).ResourceVersion != response.ResourceVersion {
		t.Fatalf("Script resourceVersion = %q", got.(*ebsv1.Script).ResourceVersion)
	}
}

func TestWriteErrorFormattingWithNilReceiver(t *testing.T) {
	var err *WriteError
	if got := err.Error(); got != "<nil>" {
		t.Fatalf("Error()=%q", got)
	}
}

func TestUpdateUsesMainResourcePath(t *testing.T) {
	request := &ebsv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "job", Namespace: "project", UID: types.UID("uid"), ResourceVersion: "1",
		},
	}
	response := request.DeepCopy()
	response.ResourceVersion = "2"
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.Method != http.MethodPut || r.URL.Path != "/apis/ebs/v1/projects/project/jobs/job" {
			return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		return jsonResponse(r, body), nil
	})

	client, err := New(testRESTConfig(transport), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := client.Update(context.Background(), schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "jobs"}, "project", request)
	if err != nil {
		t.Fatal(err)
	}
	if updated.(*ebsv1.Job).ResourceVersion != "2" {
		t.Fatalf("resourceVersion = %q, want 2", updated.(*ebsv1.Job).ResourceVersion)
	}
	if !called {
		t.Fatal("Update did not send a request")
	}
}

func TestUpdateRejectsInvalidObjectBeforeSending(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "jobs"}
	_, err := (&Client{}).Update(context.Background(), gvr, "project", nil)
	var writeErr *WriteError
	if !errors.As(err, &writeErr) || writeErr.Outcome != WriteNotSent || writeErr.Operation != "update" {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestCreateUsesResourceCollectionPath(t *testing.T) {
	request := &ebsv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "project"},
	}
	response := request.DeepCopy()
	response.UID = types.UID("uid")
	response.ResourceVersion = "1"
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/ebs/v1/projects/project/jobs" {
			return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		return jsonResponse(r, body), nil
	})
	client, err := New(testRESTConfig(transport), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.Create(context.Background(), schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "jobs"}, "project", request)
	if err != nil {
		t.Fatal(err)
	}
	if created.(*ebsv1.Job).UID != types.UID("uid") {
		t.Fatalf("UID = %q, want uid", created.(*ebsv1.Job).UID)
	}
}

func TestListProjectPageUsesScopedPathAndOptions(t *testing.T) {
	response := &ebsv1.JobList{
		TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "JobList"},
		ListMeta: metav1.ListMeta{ResourceVersion: "1"},
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/ebs/v1/projects/project/jobs" {
			return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("labelSelector") != "team=a" || query.Get("fieldSelector") != "status.phase=Pending" || query.Get("continue") != "next" || query.Get("limit") != "25" {
			return nil, fmt.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		return jsonResponse(r, body), nil
	})
	client, err := New(testRESTConfig(transport), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListProjectPage(context.Background(), schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "jobs"}, "project", metav1.ListOptions{
		LabelSelector: "team=a", FieldSelector: "status.phase=Pending", Continue: "next", Limit: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListProjectPageRejectsClusterScopedResource(t *testing.T) {
	client, err := New(testRESTConfig(nil), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListProjectPage(context.Background(), schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "runners"}, "project", metav1.ListOptions{})
	if err == nil {
		t.Fatal("project-scoped Runner List was accepted")
	}
}

func TestListPageIncludesOnlyEventFields(t *testing.T) {
	response := &ebsv1.BuildInfoList{
		TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "BuildInfoList"},
		ListMeta: metav1.ListMeta{Continue: "next"},
		Items: []ebsv1.BuildInfo{{
			ObjectMeta: metav1.ObjectMeta{Name: "buildinfo-a", Namespace: "project", UID: "uid-a", ResourceVersion: "v1:3:1"},
		}},
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/apis/ebs/v1/buildinfos" || r.URL.Query().Get("includeFields") != "metadata" || r.URL.Query().Get("continue") != "previous" || r.URL.Query().Get("limit") != "25" {
			return nil, fmt.Errorf("unexpected polling request: %s", r.URL.String())
		}
		return jsonResponse(r, body), nil
	})
	client, err := New(testRESTConfig(transport), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.ListPage(context.Background(), source.BuildInfosGVR, metav1.ListOptions{Continue: "previous", Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if page.Continue != "next" || len(page.Items) != 1 || page.Items[0].(*ebsv1.BuildInfo).Name != "buildinfo-a" {
		t.Fatalf("polling page = %#v", page)
	}
}

func TestJobHistoryListProjectionAndLiveWatchFilter(t *testing.T) {
	const liveSelector = "status.phase!=Succeeded,status.phase!=Failed,status.phase!=Aborted"
	var queries []url.Values
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		queries = append(queries, r.URL.Query())
		if r.URL.Query().Get("watch") == "true" {
			return jsonResponse(r, nil), nil
		}
		return jsonResponse(r, []byte(`{"apiVersion":"ebs/v1","kind":"JobList","metadata":{"resourceVersion":"10"},"items":[]}`)), nil
	})
	client, err := New(testRESTConfig(transport), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListPage(context.Background(), source.JobsGVR, metav1.ListOptions{
		FieldSelector: "status.phase=Succeeded", Limit: 500, Continue: "next",
	})
	if err != nil {
		t.Fatal(err)
	}
	q := queries[0]
	if q.Get("includeFields") != "metadata,status.phase,status.endTime" || q.Get("fieldSelector") != "status.phase=Succeeded" || q.Get("limit") != "500" || q.Get("continue") != "next" {
		t.Fatalf("history query = %v", q)
	}
	resource, err := client.ResolveWatch(context.Background(), source.JobsGVR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resource.ListerWatcher.List(metav1.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	stream, err := resource.ListerWatcher.Watch(metav1.ListOptions{ResourceVersion: "10"})
	if err != nil {
		t.Fatal(err)
	}
	stream.Stop()
	for _, q := range queries[1:] {
		if q.Get("fieldSelector") != liveSelector {
			t.Fatalf("live query = %v", q)
		}
	}
}

func testRESTConfig(transport http.RoundTripper) *rest.Config {
	return &rest.Config{
		Host: "http://controller-manager.test",
		WrapTransport: func(http.RoundTripper) http.RoundTripper {
			return transport
		},
	}
}

func jsonResponse(request *http.Request, body []byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    request,
	}
}
