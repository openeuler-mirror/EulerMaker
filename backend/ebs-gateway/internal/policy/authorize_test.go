package policy

import (
	"context"
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

func TestHasWatch(t *testing.T) {
	if !HasWatch(map[string][]string{"watch": {"false", "true"}}) {
		t.Fatal("repeated watch parameter bypassed check")
	}
}

func TestUserJWTOnly(t *testing.T) {
	rule := Rule{Identities: AllowUsers, UserJWTOnly: true}
	if err := rule.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, method := range []identity.AuthenticationMethod{"", identity.AuthenticationJWT, identity.AuthenticationCredential} {
		who := identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.AdminScope, AuthenticationMethod: method}
		_, err := New(nil, 1<<20).Authorize(context.Background(), who, Request{}, rule)
		if (err == nil) != (method == identity.AuthenticationJWT) {
			t.Fatalf("method=%s err=%v", method, err)
		}
	}
	for _, identities := range []RouteIdentities{AllowAnonymous, AllowUsers | AllowRunner, AllowUsers | AllowService} {
		if err := (Rule{Identities: identities, UserJWTOnly: true}).Validate(); err == nil {
			t.Fatal("login-only rule accepted non-user identities")
		}
	}
}

func TestServiceMissingGrants(t *testing.T) {
	for _, tc := range []struct {
		name   string
		grants *identity.ServiceGrants
	}{
		{"missing grants", nil},
		{"empty grants", &identity.ServiceGrants{}},
		{"missing projects", &identity.ServiceGrants{Permissions: []iamv1.MachineAccountPermission{iamv1.MachinePermissionBuildsCreate}}},
		{"missing permissions", &identity.ServiceGrants{Projects: []string{"team"}}},
		{"* without permissions", &identity.ServiceGrants{Projects: []string{iamv1.MachineAccountAllProjects}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			who := identity.Principal{Type: identity.ServiceType, Subject: "service", ServiceGrants: tc.grants}
			rule := Rule{Identities: AllowService, ServicePermission: iamv1.MachinePermissionBuildsCreate}
			_, err := New(nil, 1<<20).Authorize(context.Background(), who, Request{Project: "team"}, rule)
			if err == nil {
				t.Fatal("service without grants was allowed")
			}
		})
	}
}

func TestReadAssignedJobBody(t *testing.T) {
	const body = "{\n  \"status\": {\"runner\": \"runner-1\"}\n}"
	for _, tc := range []struct {
		name, raw string
		limit     int64
		wantError bool
	}{
		{"exact limit", body, int64(len(body)), false},
		{"over limit", body, int64(len(body) - 1), true},
		{"multiple objects", body + "{}", int64(len(body) + 2), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := upstream.New("https://api.example", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/apis/ebs/v1/projects/team/jobs/job-1/status" || r.URL.RawQuery != "" {
					t.Fatalf("unexpected lookup: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.raw))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := New(client, tc.limit).ReadAssignedJob(context.Background(), "/apis/ebs/v1/projects/team/jobs/job-1/status", "runner-1", true)
			if tc.wantError {
				if classified, ok := err.(*Error); !ok || classified.Status != http.StatusBadGateway {
					t.Fatalf("expected invalid upstream body: %v", err)
				}
			} else if err != nil || string(raw) != body {
				t.Fatalf("raw=%s err=%v", raw, err)
			}
		})
	}
}

func TestAuthorizeObject(t *testing.T) {
	client, err := upstream.New("https://api.example", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice","ebs.io/member-user.bob":"true"}}}`
		if strings.Contains(r.URL.Path, "/jobs/") {
			body = `{"status":{"runner":"runner-1"}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := New(client, 1<<20)
	for _, tc := range []struct {
		name                string
		who                 identity.Principal
		access, watchAccess ObjectAccess
		watch               bool
		want                int
		wantJob             bool
	}{
		{"owner", identity.Principal{Type: identity.UserType, Subject: "alice", Scope: identity.UserScope}, ProjectOwner, "", false, 0, false},
		{"member not owner", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, ProjectOwner, "", false, 403, false},
		{"admin member", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.AdminScope}, ProjectMember, "", false, 0, false},
		{"admin outsider", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, ProjectMember, "", false, 403, false},
		{"public list", identity.Principal{}, "", ProjectMember, false, 0, false},
		{"watch member", identity.Principal{Type: identity.UserType, Subject: "bob", Scope: identity.UserScope}, "", ProjectMember, true, 0, false},
		{"watch outsider", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}, "", ProjectMember, true, 403, false},
		{"self", identity.Principal{Type: identity.RunnerType, Subject: "runner-1"}, RunnerSelf, "", false, 0, false},
		{"other runner", identity.Principal{Type: identity.RunnerType, Subject: "runner-2"}, RunnerSelf, "", false, 403, false},
		{"ops manages runner", identity.Principal{Type: identity.UserType, Subject: "ops", Scope: identity.OpsScope}, RunnerSelf, "", false, 0, false},
		{"assigned job", identity.Principal{Type: identity.RunnerType, Subject: "runner-1"}, AssignedJob, "", false, 0, true},
		{"other job", identity.Principal{Type: identity.RunnerType, Subject: "runner-2"}, AssignedJob, "", false, 403, false},
		{"public job", identity.Principal{}, AssignedJob, "", false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := Request{Method: http.MethodPatch, Project: "team", Runner: "runner-1", Path: "/apis/ebs/v1/projects/team/jobs/job-1", Watch: tc.watch}
			rule := Rule{Identities: AllowAnonymous | AllowUsers | AllowRunner, ObjectAccess: tc.access, WatchAccess: tc.watchAccess}
			result, err := a.Authorize(context.Background(), tc.who, request, rule)
			status := 0
			if err != nil {
				status = err.(*Error).Status
			}
			if status != tc.want || (err == nil && (len(result.UpdateObject) > 0) != tc.wantJob) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestAuthorizeService(t *testing.T) {
	permissions := []iamv1.MachineAccountPermission{
		iamv1.MachinePermissionProjectsUpdate, iamv1.MachinePermissionBuildsCreate, iamv1.MachinePermissionBuildsAbort,
	}
	a := New(nil, 1<<20)
	for _, projects := range [][]string{{"team"}, {iamv1.MachineAccountAllProjects}, {"all"}} {
		for _, required := range permissions {
			for _, granted := range permissions {
				for _, project := range []string{"team", "other", ""} {
					who := identity.Principal{
						Type: identity.ServiceType, Subject: "sync",
						ServiceGrants: &identity.ServiceGrants{Projects: projects, Permissions: []iamv1.MachineAccountPermission{granted}},
					}
					rule := Rule{Identities: AllowService, ServicePermission: required, ObjectAccess: ProjectOwner}
					_, err := a.Authorize(context.Background(), who, Request{Project: project}, rule)
					allowedProject := project != "" && (projects[0] == "*" || (projects[0] == "team" && project == "team"))
					if (err == nil) != (required == granted && allowedProject) {
						t.Errorf("projects=%v required=%s granted=%s project=%q err=%v", projects, required, granted, project, err)
					}
				}
			}
		}
	}
}
