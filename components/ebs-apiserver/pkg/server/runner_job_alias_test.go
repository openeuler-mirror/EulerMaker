package server

import (
	"net/http/httptest"
	"testing"
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
