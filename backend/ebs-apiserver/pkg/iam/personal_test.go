package iam

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	iamv1 "ebs-api/iam/v1"
	"ebs-apiserver/pkg/iam/credential"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
)

func (f *oauthFixture) personalCall(method, path, principal, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/internal/iam/v1/users/"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Headers must not stand in for the apiserver's authenticated component identity.
	req.Header.Set("X-EBS-User", gatewayPrincipal)
	if principal != "" {
		req = req.WithContext(request.WithUser(req.Context(), &user.DefaultInfo{Name: principal}))
	}
	rec := httptest.NewRecorder()
	f.routes.ServeHTTP(rec, req)
	return rec
}

func personalFixture(t *testing.T) *oauthFixture {
	t.Helper()
	f := newOAuthFixture(t)
	assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("alice")), 201, "")
	return f
}

func createPersonal(t *testing.T, f *oauthFixture) credential.CreatedPersonalCredential {
	t.Helper()
	rec := f.personalCall("POST", "alice/credentials", gatewayPrincipal, `{"name":"开发机"}`)
	assertOAuthResponse(t, rec, 201, "")
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}
	var result credential.CreatedPersonalCredential
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Token == "" {
		t.Fatal("missing credential")
	}
	if strings.Contains(rec.Body.String(), "secretHash") {
		t.Fatal("create exposed hash")
	}
	return result
}

func personalAuthBody(token string) string {
	body, _ := json.Marshal(map[string]string{"method": "credential", "token": token})
	return string(body)
}

func TestPersonalCredentialRoutes(t *testing.T) {
	f := personalFixture(t)
	c := createPersonal(t, f)
	if c.ExpiresAt.Sub(c.CreatedAt) != 90*24*time.Hour {
		t.Fatal("incorrect default lifetime")
	}
	rec := f.personalCall("POST", "authenticate", gatewayPrincipal, personalAuthBody(c.Token))
	assertOAuthResponse(t, rec, 200, "")
	var auth credential.PersonalAuthentication
	if err := json.Unmarshal(rec.Body.Bytes(), &auth); err != nil || !auth.Authenticated || auth.Username != "alice" || auth.UID == "" || len(auth.Scopes) != 1 || auth.Scopes[0] != "ebs:user" || auth.CredentialID != c.ID || !auth.ExpiresAt.Equal(c.ExpiresAt) {
		t.Fatal("invalid authentication response")
	}
	rec = f.personalCall("GET", "alice/credentials", gatewayPrincipal, "")
	assertOAuthResponse(t, rec, 200, "")
	if !strings.Contains(rec.Body.String(), c.ID) || strings.Contains(rec.Body.String(), "secretHash") || strings.Contains(rec.Body.String(), "token") {
		t.Fatal("invalid credential list")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("list can be cached")
	}
	assertOAuthResponse(t, f.call("register", gatewayPrincipal, `{"username":"bob","password":"correct password"}`), 201, "")
	rec = f.personalCall("DELETE", "bob/credentials/"+c.ID, gatewayPrincipal, "")
	if rec.Code != 204 {
		t.Fatal("delete absent credential failed")
	}
	assertOAuthResponse(t, f.personalCall("POST", "authenticate", gatewayPrincipal, personalAuthBody(c.Token)), 200, "")
	for range 4 {
		createPersonal(t, f)
	}
	assertOAuthResponse(t, f.personalCall("POST", "alice/credentials", gatewayPrincipal, `{"name":"sixth"}`), 409, "CredentialLimitExceeded")
	for range 2 {
		rec = f.personalCall("DELETE", "alice/credentials/"+c.ID, gatewayPrincipal, "")
		if rec.Code != 204 || rec.Body.Len() != 0 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("delete: %d %s", rec.Code, rec.Body)
		}
	}
	assertOAuthResponse(t, f.personalCall("POST", "authenticate", gatewayPrincipal, personalAuthBody(c.Token)), 401, "InvalidCredential")
	createPersonal(t, f)
}

func TestPersonalCredentialRequiresGateway(t *testing.T) {
	f := personalFixture(t)
	for _, principal := range []string{"", "system:anonymous", "alice", "spiffe://eulermaker/internal/controller-manager", "spiffe://eulermaker/internal/scheduler"} {
		for _, route := range []struct{ method, path, body string }{
			{"POST", "alice/credentials", `{"name":"cli"}`},
			{"GET", "alice/credentials", ""},
			{"DELETE", "alice/credentials/" + strings.Repeat("a", 32), ""},
			{"POST", "authenticate", personalAuthBody("not-a-token")},
		} {
			before := len(f.backend.requests)
			rec := f.personalCall(route.method, route.path, principal, route.body)
			status, code := 403, "Forbidden"
			if principal == "" || principal == "system:anonymous" {
				status, code = 401, "Unauthorized"
			}
			assertOAuthResponse(t, rec, status, code)
			if len(f.backend.requests) != before {
				t.Fatal("unauthorized request reached storage")
			}
		}
	}
}

func TestPersonalCredentialRequestValidation(t *testing.T) {
	f := personalFixture(t)
	for _, body := range []string{
		`{"name":"\`, `{"name":"\u`, `{"name":"\u12`,
		`null`, `[]`, `{}`, `{"name":null}`, `{"name":""}`, `{"name":"a\nb"}`, `{"name":"cli","expiresInDays":null}`,
		`{"name":"cli","expiresInDays":0}`, `{"name":"cli","expiresInDays":91}`, `{"name":"cli","expiresInDays":1.5}`,
		`{"name":"cli","expiresInDays":"1"}`, `{"name":"cli","expiresInDays":true}`,
		`{"name":"cli","username":"bob"}`, `{"name":"cli","id":"chosen"}`, `{"name":"cli","secretHash":"chosen"}`,
		`{"name":"cli","scopes":["ebs:admin"]}`, `{"name":"cli","createdAt":"yesterday"}`, `{"name":"cli"} {}`,
		`{"name":"` + strings.Repeat("字", 65) + `"}`, `{"name":"\ud800"}`, `{"name":"` + string([]byte{0xff}) + `"}`,
		`{"name":"cli"}` + strings.Repeat(" ", maxRequestBody),
	} {
		before := len(f.backend.requests)
		assertOAuthResponse(t, f.personalCall("POST", "alice/credentials", gatewayPrincipal, body), 400, "InvalidRequest")
		if len(f.backend.requests) != before {
			t.Fatal("invalid input reached storage")
		}
	}
	for _, body := range []string{
		`{"method":"credential"}`, `{"method":"credential","token":null}`, `{"method":"credential","token":12}`,
		`{"method":"credential","token":"x","username":"alice"}`, `{"method":"credential","token":"x","password":"x"}`,
		`{"method":"credential","token":"x","externalIdentity":{}}`,
	} {
		assertOAuthResponse(t, f.personalCall("POST", "authenticate", gatewayPrincipal, body), 400, "InvalidRequest")
	}
	assertOAuthResponse(t, f.personalCall("POST", "register", gatewayPrincipal, personalAuthBody("x")), 400, "InvalidRequest")
	for _, method := range []string{"GET", "DELETE"} {
		path := "alice/credentials"
		if method == "DELETE" {
			path += "/" + strings.Repeat("a", 32)
		}
		assertOAuthResponse(t, f.personalCall(method, path, gatewayPrincipal, `{}`), 400, "InvalidRequest")
	}
	assertOAuthResponse(t, f.personalCall("DELETE", "alice/credentials/bad-id", gatewayPrincipal, ""), 400, "InvalidRequest")
	assertOAuthResponse(t, f.personalCall("POST", "authenticate", gatewayPrincipal, personalAuthBody("invalid")), 401, "InvalidCredential")
	assertOAuthResponse(t, f.personalCall("GET", "missing/credentials", gatewayPrincipal, ""), 404, "UserNotFound")
}

func TestPersonalCredentialStorageFailureDoesNotLeakOrRetry(t *testing.T) {
	for _, committed := range []bool{false, true} {
		f := personalFixture(t)
		writes := 0
		f.backend.fail = func(r *http.Request, after bool) bool {
			if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "ebs-users") && after == committed {
				writes++
				return true
			}
			return false
		}
		rec := f.personalCall("POST", "alice/credentials", gatewayPrincipal, `{"name":"cli"}`)
		assertOAuthResponse(t, rec, 503, "CredentialServiceUnavailable")
		if writes != 1 || strings.Contains(rec.Body.String(), "ebs_pat_") || strings.Contains(rec.Body.String(), "secretHash") {
			t.Fatal("unknown write retried or secret leaked")
		}
		f.backend.fail = nil
		items, err := f.creds.ListPersonal(context.Background(), "alice")
		want := 0
		if committed {
			want = 1
		}
		if err != nil || len(items) != want {
			t.Fatalf("stored count=%d want=%d: %v", len(items), want, err)
		}
	}
	f := personalFixture(t)
	c := createPersonal(t, f)
	f.backend.fail = func(r *http.Request, _ bool) bool { return r.Method == http.MethodGet }
	assertOAuthResponse(t, f.personalCall("POST", "authenticate", gatewayPrincipal, personalAuthBody(c.Token)), 503, "CredentialServiceUnavailable")
	assertOAuthResponse(t, f.personalCall("GET", "alice/credentials", gatewayPrincipal, ""), 503, "CredentialServiceUnavailable")
}

func TestPersonalCredentialPublicUserUpdatePreservesPrivateData(t *testing.T) {
	f := personalFixture(t)
	c := createPersonal(t, f)
	ctx := request.WithNamespace(context.Background(), "")
	obj, err := f.users.Get(ctx, "alice", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(obj)
	if strings.Contains(string(data), "personalCredentials") || strings.Contains(string(data), "secretHash") {
		t.Fatal("public User exposes credentials")
	}
	u := obj.(*iamv1.User)
	u.Spec.DisplayName = "new name"
	if _, _, err := f.users.Update(ctx, "alice", rest.DefaultUpdatedObjectInfo(u), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.creds.AuthenticatePersonal(ctx, c.Token); err != nil {
		t.Fatal("public update lost PAT", err)
	}
	if f.userDocument(t, "alice").ExternalIdentity == nil {
		t.Fatal("public update lost OAuth identity")
	}
}
