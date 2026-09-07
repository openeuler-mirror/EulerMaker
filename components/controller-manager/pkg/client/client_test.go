package client

import (
	"errors"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

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
