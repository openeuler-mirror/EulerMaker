package client

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

	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
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
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    r,
		}, nil
	})

	client, err := New(&rest.Config{
		Host: "http://controller-manager.test",
		WrapTransport: func(http.RoundTripper) http.RoundTripper {
			return transport
		},
	}, time.Second)
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
