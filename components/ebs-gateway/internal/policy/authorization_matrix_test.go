package policy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func projectAuthorizer(t *testing.T) *Authorizer {
	t.Helper()
	client, err := upstream.New("https://api.example", transportFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/apis/ebs/v1/projects/team" {
			t.Fatalf("unexpected Project lookup: %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice","ebs.io/member-user.bob":"true"}}}`))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return New(client)
}

func TestProjectAuthorizationMatrix(t *testing.T) {
	a := projectAuthorizer(t)
	tests := []struct {
		name  string
		who   identity.Principal
		route Route
		allow bool
	}{
		{"owner updates Project", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, true},
		{"owner deletes Project", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}, Route{Resource: "projects", Name: "team", Method: http.MethodDelete}, true},
		{"ops owner updates Project", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.OpsScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, true},
		{"member cannot update Project", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, false},
		{"admin may create Project", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "projects", Method: http.MethodPost}, true},
		{"admin cannot update Project", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPut}, false},
		{"admin cannot update Project even with owner name", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.AdminScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, false},
		{"admin cannot update Project status", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "projects", Name: "team", Subresource: "status", Method: http.MethodPatch}, false},
		{"admin cannot delete Project", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "projects", Name: "team", Method: http.MethodDelete}, false},
		{"member creates Job", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Method: http.MethodPost}, true},
		{"member cannot delete Job", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Name: "j", Method: http.MethodDelete}, false},
		{"outsider cannot create Job", identity.Principal{Type: identity.UserType, Subject: "charlie", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Method: http.MethodPost}, false},
		{"ops without membership cannot write", identity.Principal{Type: identity.UserType, Subject: "ops", Scope: identity.OpsScope}, Route{Resource: "jobs", Project: "team", Method: http.MethodPost}, false},
		{"admin cannot update Job status", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "status", Method: http.MethodPatch}, false},
		{"owner may abort Job", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "abort", Method: http.MethodPost}, true},
		{"member may abort Job", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "abort", Method: http.MethodPost}, true},
		{"outsider admin cannot abort Job", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "abort", Method: http.MethodPost}, false},
		{"Build PUT is never exposed", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "builds", Project: "team", Name: "b", Method: http.MethodPut}, false},
		{"admin cannot create Snapshot", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "snapshots", Project: "team", Method: http.MethodPost}, false},
		{"owner cannot update BuildInfo", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}, Route{Resource: "buildinfos", Project: "team", Name: "b", Method: http.MethodPut}, false},
		{"admin cannot update RpmRepo status", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "rpmrepos", Project: "team", Name: "r", Subresource: "status", Method: http.MethodPatch}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := a.Authorize(context.Background(), test.who, test.route)
			if (err == nil) != test.allow {
				t.Fatalf("Authorize() error = %v, want allow=%v", err, test.allow)
			}
		})
	}
}

func TestRunnerAndScriptAuthorization(t *testing.T) {
	a := projectAuthorizer(t)
	runner := identity.Principal{Type: identity.RunnerType, Subject: "runner-1", Runner: "runner-1"}
	for _, test := range []struct {
		route Route
		allow bool
	}{
		{Route{Resource: "runners", Name: "runner-1", Method: http.MethodPatch}, true},
		{Route{Resource: "runners", Name: "runner-2", Method: http.MethodPatch}, false},
		{Route{Resource: "runners", Method: http.MethodGet}, false},
		{Route{Resource: "runners", Name: "runner-1", Subresource: "jobs", Method: http.MethodGet}, true},
		{Route{Resource: "jobs", Project: "team", Name: "job-1", Method: http.MethodGet}, true},
		{Route{Resource: "jobs", Project: "team", Name: "job-1", Subresource: "status", Method: http.MethodPatch}, true},
		{Route{Resource: "jobs", Project: "team", Name: "job-1", Subresource: "status", Method: http.MethodGet}, false},
		{Route{Resource: "scripts", Name: "rpmbuild", Method: http.MethodGet}, true},
		{Route{Resource: "scripts", Method: http.MethodGet}, false},
	} {
		err := a.Authorize(context.Background(), runner, test.route)
		if (err == nil) != test.allow {
			t.Errorf("Authorize(%+v) error=%v, want allow=%v", test.route, err, test.allow)
		}
	}
}
