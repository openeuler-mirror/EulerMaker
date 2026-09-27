package handler

import (
	"net/http"
	"net/url"
	"testing"

	"ebs-gateway/internal/policy"
)

func TestResourceMethodBoundary(t *testing.T) {
	tests := []struct {
		name      string
		route     policy.Route
		wantAllow bool
	}{
		{"collection list", policy.Route{Resource: "projects", Method: http.MethodGet}, true},
		{"collection update", policy.Route{Resource: "projects", Method: http.MethodPut}, false},
		{"named create", policy.Route{Resource: "projects", Name: "p", Method: http.MethodPost}, false},
		{"status read", policy.Route{Resource: "jobs", Name: "j", Subresource: "status", Method: http.MethodGet}, true},
		{"status delete", policy.Route{Resource: "jobs", Name: "j", Subresource: "status", Method: http.MethodDelete}, false},
		{"abort post", policy.Route{Resource: "jobs", Name: "j", Subresource: "abort", Method: http.MethodPost}, true},
		{"abort get", policy.Route{Resource: "jobs", Name: "j", Subresource: "abort", Method: http.MethodGet}, false},
		{"runner jobs watch", policy.Route{Resource: "runners", Name: "r", Subresource: "jobs", Method: http.MethodGet}, true},
		{"runner jobs mutate", policy.Route{Resource: "runners", Name: "r", Subresource: "jobs", Method: http.MethodPatch}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := allowedResourceMethod(test.route); got != test.wantAllow {
				t.Fatalf("allowedResourceMethod() = %v, want %v", got, test.wantAllow)
			}
		})
	}
}

func TestRunnerJobsQueryRejectsCallerFieldSelector(t *testing.T) {
	for _, test := range []struct {
		query url.Values
		allow bool
	}{
		{url.Values{"watch": {"true"}, "resourceVersion": {"7"}, "timeoutSeconds": {"300"}}, true},
		{url.Values{"limit": {"100"}, "continue": {"next"}}, true},
		{url.Values{"fieldSelector": {"status.runner=other"}}, false},
		{url.Values{"watch": {"true", "false"}}, false},
		{url.Values{"timeoutSeconds": {"301"}}, false},
	} {
		if got := validRunnerJobsQuery(test.query); got != test.allow {
			t.Errorf("validRunnerJobsQuery(%v) = %v, want %v", test.query, got, test.allow)
		}
	}
}
