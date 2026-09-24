package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	ebsv1 "ebs-api/ebs/v1"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
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
	jobs := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "jobs"}
	runners := schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "runners"}
	if err := validateTarget(jobs, "", "job"); err == nil {
		t.Fatal("namespace-scoped target accepted an empty namespace")
	}
	if err := validateTarget(runners, "project", "runner"); err == nil {
		t.Fatal("cluster-scoped target accepted a namespace")
	}
	if err := validateTarget(jobs, "project", "job"); err != nil {
		t.Fatal(err)
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
	_, err := (&Client{}).ListProjectPage(context.Background(), schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "runners"}, "project", metav1.ListOptions{})
	if err == nil {
		t.Fatal("project-scoped Runner List was accepted")
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
