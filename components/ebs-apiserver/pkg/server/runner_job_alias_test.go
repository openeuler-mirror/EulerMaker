package server

import (
	"context"
	"net/http/httptest"
	"testing"

	apirequest "k8s.io/apiserver/pkg/endpoints/request"
)

func TestRunnerJobIdentityAllowed(t *testing.T) {
	tests := []struct {
		name   string
		user   string
		scopes string
		want   bool
	}{
		{name: "matching runner", user: "runner-a", scopes: "ebs:runner", want: true},
		{name: "other runner", user: "runner-b", scopes: "ebs:runner"},
		{name: "system", user: "scheduler", scopes: "ebs:system", want: true},
		{name: "user", user: "runner-a", scopes: "ebs:user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("X-EBS-User", tt.user)
			req.Header.Set("X-EBS-Scopes", tt.scopes)
			if got := runnerJobIdentityAllowed(req, "runner-a"); got != tt.want {
				t.Fatalf("allowed=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestGlobalJobRequestContextRemovesAliasNameScope(t *testing.T) {
	original := &apirequest.RequestInfo{
		IsResourceRequest: true,
		Path:              "/apis/ebs/v1/runners/runner-a/jobs",
		Verb:              "watch",
		APIPrefix:         "apis",
		APIGroup:          "ebs",
		APIVersion:        "v1",
		Resource:          "runners",
		Name:              "runner-a",
		Subresource:       "jobs",
	}
	ctx := apirequest.WithRequestInfo(context.Background(), original)
	ctx = globalJobRequestContext(ctx, "/apis/ebs/v1/jobs")

	got, ok := apirequest.RequestInfoFrom(ctx)
	if !ok || got == nil {
		t.Fatal("rewritten request info is missing")
	}
	if got.Path != "/apis/ebs/v1/jobs" || got.Resource != "jobs" || got.Name != "" || got.Subresource != "" {
		t.Fatalf("unexpected rewritten request info: %#v", got)
	}
	if got.Verb != "watch" || got.APIGroup != "ebs" || got.APIVersion != "v1" {
		t.Fatalf("rewritten request info lost API identity: %#v", got)
	}
	if original.Resource != "runners" || original.Name != "runner-a" || original.Subresource != "jobs" {
		t.Fatalf("original request info was mutated: %#v", original)
	}
}

func TestRunnerJobWatchIsLongRunning(t *testing.T) {
	check := runnerJobLongRunningCheck(nil)
	info := &apirequest.RequestInfo{Verb: "get", IsResourceRequest: true, Resource: "runners", Name: "runner-a", Subresource: "jobs"}
	tests := []struct {
		path string
		want bool
	}{
		{"/apis/ebs/v1/runners/runner-a/jobs?watch=true", true},
		{"/apis/ebs/v1/runners/runner-a/jobs?watch=1", true},
		{"/apis/ebs/v1/runners/runner-a/jobs?watch=false", false},
		{"/apis/ebs/v1/runners/runner-a/jobs", false},
		{"/apis/ebs/v1/runners/runner-a/status?watch=true", false},
	}
	for _, test := range tests {
		req := httptest.NewRequest("GET", test.path, nil)
		if got := check(req, info); got != test.want {
			t.Errorf("check(%q)=%v want %v", test.path, got, test.want)
		}
	}
}
