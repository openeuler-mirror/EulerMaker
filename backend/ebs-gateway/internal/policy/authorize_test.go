package policy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	iamv1 "ebs-api/iam/v1"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestProjectAuthorizationMatrix(t *testing.T) {
	a := projectAuthorizer(t)
	tests := []struct {
		name  string
		who   identity.Principal
		route Route
		allow bool
	}{
		{"owner updates Project", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, true},
		{"owner cannot delete Project", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}, Route{Resource: "projects", Name: "team", Method: http.MethodDelete}, false},
		{"ops owner cannot delete Project", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.OpsScope}, Route{Resource: "projects", Name: "team", Method: http.MethodDelete}, false},
		{"ops owner updates Project", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.OpsScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, true},
		{"member cannot update Project", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, false},
		{"admin may create Project", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "projects", Method: http.MethodPost}, true},
		{"admin cannot update Project", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPut}, false},
		{"admin owner may reach purpose validation", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.AdminScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, true},
		{"ops non-owner cannot update Project", identity.Principal{Type: identity.UserType, Subject: "ops", Scope: identity.OpsScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, false},
		{"admin cannot update Project status", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "projects", Name: "team", Subresource: "status", Method: http.MethodPatch}, false},
		{"admin cannot delete Project", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "projects", Name: "team", Method: http.MethodDelete}, false},
		{"member creates Job", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Method: http.MethodPost}, true},
		{"owner deletes Job", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Name: "j", Method: http.MethodDelete}, true},
		{"member updates Job", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Name: "j", Method: http.MethodPut}, true},
		{"member cannot delete Job", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Name: "j", Method: http.MethodDelete}, false},
		{"outsider cannot create Job", identity.Principal{Type: identity.UserType, Subject: "charlie", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Method: http.MethodPost}, false},
		{"ops without membership cannot write", identity.Principal{Type: identity.UserType, Subject: "ops", Scope: identity.OpsScope}, Route{Resource: "jobs", Project: "team", Method: http.MethodPost}, false},
		{"admin cannot update Job status", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "status", Method: http.MethodPatch}, false},
		{"owner may abort Job", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "abort", Method: http.MethodPost}, true},
		{"member may abort Job", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "abort", Method: http.MethodPost}, true},
		{"outsider admin cannot abort Job", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "abort", Method: http.MethodPost}, false},
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

func TestAuthorizationLookupBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		who    identity.Principal
		route  Route
		lookup bool
		status int
	}{
		{"admin creates Build", identity.Principal{Type: identity.UserType, Scope: identity.AdminScope}, Route{Resource: "builds", Project: "team", Method: http.MethodPost}, false, 0},
		{"admin deletes Build", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "builds", Project: "team", Name: "b", Method: http.MethodDelete}, true, 503},
		{"admin updates Project", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, true, 503},
		{"user creates Project", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}, Route{Resource: "projects", Method: http.MethodPost}, false, 0},
		{"admin aborts Job", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "abort", Method: http.MethodPost}, true, 503},
		{"admin updates Job status", identity.Principal{Type: identity.UserType, Scope: identity.AdminScope}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "status", Method: http.MethodPatch}, false, 403},
		{"runner reads Job", identity.Principal{Type: identity.RunnerType}, Route{Resource: "jobs", Project: "team", Name: "j", Method: http.MethodGet}, false, 0},
		{"runner updates Job status", identity.Principal{Type: identity.RunnerType}, Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "status", Method: http.MethodPatch}, false, 0},
		{"runner deletes Job", identity.Principal{Type: identity.RunnerType}, Route{Resource: "jobs", Project: "team", Name: "j", Method: http.MethodDelete}, false, 403},
		{"runner reads Snapshot", identity.Principal{Type: identity.RunnerType}, Route{Resource: "snapshots", Project: "team", Name: "s", Method: http.MethodGet}, false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups := 0
			client, err := upstream.New("https://api.example", roundTripFunc(func(*http.Request) (*http.Response, error) {
				lookups++
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			err = New(client).Authorize(context.Background(), tc.who, tc.route)
			if (lookups == 1) != tc.lookup || lookups > 1 {
				t.Fatalf("lookups=%d, want lookup=%v", lookups, tc.lookup)
			}
			if tc.status == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var classified *Error
			if !errors.As(err, &classified) || classified.Status != tc.status {
				t.Fatalf("error=%v, want status %d", err, tc.status)
			}
		})
	}
}

func TestReadOnlyResourceAuthorization(t *testing.T) {
	a := projectAuthorizer(t)
	owner := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}
	for _, resource := range []string{"snapshots", "buildinfos", "buildgraphs", "rpmrepos", "oldjobs"} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			t.Run(resource+"/"+method, func(t *testing.T) {
				route := Route{Resource: resource, Project: "team", Name: "item", Method: method}
				if err := a.Authorize(context.Background(), owner, route); (err == nil) != (method == http.MethodGet) {
					t.Fatalf("Authorize() error=%v, want allow=%v", err, method == http.MethodGet)
				}
			})
		}
	}
}

func TestBuildDeletionRequiresProjectOwner(t *testing.T) {
	a := projectAuthorizer(t)
	route := Route{Resource: "builds", Project: "team", Name: "b", Method: http.MethodDelete}
	for _, scope := range []identity.Scope{identity.UserScope, identity.OpsScope, identity.AdminScope} {
		for _, subject := range []string{"alice", "bob", "charlie"} {
			t.Run(string(scope)+"/"+subject, func(t *testing.T) {
				who := identity.Principal{Type: identity.UserType, Subject: subject, Scope: scope}
				err := a.Authorize(context.Background(), who, route)
				if subject == "alice" {
					if err != nil {
						t.Fatalf("owner deletion denied: %v", err)
					}
					return
				}
				if denied, ok := err.(*Error); !ok || denied.Status != http.StatusForbidden {
					t.Fatalf("non-owner deletion error = %v, want Forbidden", err)
				}
			})
		}
	}
}

func TestPublicRead(t *testing.T) {
	if !IsPublicRead(Route{Resource: "jobs", Project: "team", Method: http.MethodGet}) {
		t.Fatal("Project-scoped Job list should be public")
	}
	if IsPublicRead(Route{Resource: "runners", Method: http.MethodGet}) {
		t.Fatal("Runner list must not be public")
	}
	if !HasWatch(map[string][]string{"watch": {"false", "true"}}) {
		t.Fatal("repeated watch parameter bypassed check")
	}
}

func TestServiceOperationAllowlist(t *testing.T) {
	a := &Authorizer{}
	who := identity.Principal{
		Type: identity.ServiceType, Subject: "automation",
		ServiceGrants: &identity.ServiceGrants{
			Projects: []string{"team"},
			Permissions: []iamv1.MachineAccountPermission{
				iamv1.MachinePermissionProjectsUpdate, iamv1.MachinePermissionBuildsCreate, iamv1.MachinePermissionBuildsAbort,
			},
		},
	}
	for _, test := range []struct {
		route Route
		allow bool
	}{
		{Route{Resource: "projects", Name: "team", Method: http.MethodPut}, true},
		{Route{Resource: "projects", Name: "team", Method: http.MethodPatch}, true},
		{Route{Resource: "projects", Name: "other", Method: http.MethodPatch}, false},
		{Route{Resource: "projects", Name: "team", Method: http.MethodGet}, false},
		{Route{Resource: "projects", Name: "team", Subresource: "status", Method: http.MethodPut}, false},
		{Route{Resource: "projects", Method: http.MethodPost}, false},
		{Route{Resource: "projects", Method: http.MethodPut}, false},
		{Route{Resource: "builds", Project: "team", Method: http.MethodPost}, true},
		{Route{Resource: "builds", Project: "other", Method: http.MethodPost}, false},
		{Route{Resource: "builds", Method: http.MethodPost}, false},
		{Route{Resource: "builds", Project: "team", Name: "b", Method: http.MethodPost}, false},
		{Route{Resource: "builds", Project: "team", Name: "b", Subresource: "abort", Method: http.MethodPost}, true},
		{Route{Resource: "builds", Project: "other", Name: "b", Subresource: "abort", Method: http.MethodPost}, false},
		{Route{Resource: "builds", Project: "team", Subresource: "abort", Method: http.MethodPost}, false},
		{Route{Resource: "jobs", Project: "team", Name: "j", Subresource: "abort", Method: http.MethodPost}, false},
		{Route{Resource: "jobs", Project: "team", Method: http.MethodPost}, false},
		{Route{Resource: "machineaccounts", Method: http.MethodGet}, false},
	} {
		err := a.Authorize(context.Background(), who, test.route)
		if (err == nil) != test.allow {
			t.Errorf("Authorize(%+v) = %v, want allowed=%v", test.route, err, test.allow)
		}
	}
}

func TestServicePermissionsAreIndependent(t *testing.T) {
	operations := []struct {
		permission iamv1.MachineAccountPermission
		route      Route
	}{
		{iamv1.MachinePermissionProjectsUpdate, Route{Resource: "projects", Name: "team", Method: http.MethodPatch}},
		{iamv1.MachinePermissionBuildsCreate, Route{Resource: "builds", Project: "team", Method: http.MethodPost}},
		{iamv1.MachinePermissionBuildsAbort, Route{Resource: "builds", Project: "team", Name: "b", Subresource: "abort", Method: http.MethodPost}},
	}
	for _, granted := range operations {
		who := identity.Principal{Type: identity.ServiceType, ServiceGrants: &identity.ServiceGrants{
			Projects: []string{"team"}, Permissions: []iamv1.MachineAccountPermission{granted.permission},
		}}
		for _, requested := range operations {
			err := (&Authorizer{}).Authorize(context.Background(), who, requested.route)
			if (err == nil) != (granted.permission == requested.permission) {
				t.Errorf("granted=%s requested=%s err=%v", granted.permission, requested.permission, err)
			}
		}
	}
	for _, grants := range []*identity.ServiceGrants{nil, {}, {Projects: []string{"team"}}, {
		Permissions: []iamv1.MachineAccountPermission{iamv1.MachinePermissionProjectsUpdate},
	}} {
		who := identity.Principal{Type: identity.ServiceType, ServiceGrants: grants}
		if err := (&Authorizer{}).Authorize(context.Background(), who, operations[0].route); err == nil {
			t.Errorf("incomplete grants allowed: %+v", grants)
		}
	}
}

func TestRunnerAuthorization(t *testing.T) {
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
	} {
		err := a.Authorize(context.Background(), runner, test.route)
		if (err == nil) != test.allow {
			t.Errorf("Authorize(%+v) error=%v, want allow=%v", test.route, err, test.allow)
		}
	}
}

func TestScriptAuthorization(t *testing.T) {
	runner := identity.Principal{Type: identity.RunnerType, Subject: "runner-1", Runner: "runner-1"}
	for _, tc := range []struct {
		who    identity.Principal
		name   string
		method string
		allow  bool
	}{
		{runner, "rpmbuild", http.MethodGet, true},
		{runner, "", http.MethodGet, false},
		{runner, "rpmbuild", http.MethodPut, false},
		{identity.Principal{Type: identity.ServiceType}, "rpmbuild", http.MethodGet, false},
	} {
		route := Route{Resource: "scripts", Name: tc.name, Method: tc.method}
		if err := AuthorizeScript(context.Background(), tc.who, route); (err == nil) != tc.allow {
			t.Errorf("AuthorizeScript(%s, %+v) error=%v, want allow=%v", tc.who.Type, route, err, tc.allow)
		}
	}
}

func TestConfigAuthorization(t *testing.T) {
	ordinary := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}
	ops := identity.Principal{Type: identity.UserType, Subject: "ops", Scope: identity.OpsScope}
	admin := identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}
	runner := identity.Principal{Type: identity.RunnerType, Subject: "runner-a", Runner: "runner-a"}
	for _, test := range []struct {
		who    identity.Principal
		name   string
		method string
		allow  bool
	}{
		{ordinary, "services", http.MethodGet, false},
		{ordinary, "services", http.MethodPut, false},
		{runner, "services", http.MethodGet, false},
		{identity.Principal{Type: identity.ServiceType}, "services", http.MethodGet, false},
		{ops, "services", http.MethodGet, true},
		{ops, "services", http.MethodPost, true},
		{ops, "services", http.MethodPut, true},
		{ops, "services", http.MethodPatch, true},
		{admin, "services", http.MethodGet, true},
		{admin, "services", http.MethodPut, true},
		{admin, "services", http.MethodDelete, false},
		{ops, "build-target", http.MethodDelete, false},
		{ops, "build-resource", http.MethodDelete, false},
		{ops, "services", http.MethodDelete, false},
		{ops, "custom", http.MethodDelete, true},
	} {
		route := Route{Resource: "configs", Name: test.name, Method: test.method}
		if err := AuthorizeConfig(context.Background(), test.who, route); (err == nil) != test.allow {
			t.Errorf("Authorize(%s, %s, %s) error=%v, want allow=%v", test.who.Scope, test.name, test.method, err, test.allow)
		}
	}
}
