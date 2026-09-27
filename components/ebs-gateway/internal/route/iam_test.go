package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ebs-gateway/internal/identity"
)

func TestAdminUserGetUsesCheckedResponse(t *testing.T) {
	calls := 0
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/apis/iam.ebs/v1/users/admin":
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`))}, nil
		case "/apis/iam.ebs/v1/users/alice":
			calls++
			scope := "ebs:user"
			if calls > 1 {
				scope = "ebs:admin"
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["` + scope + `"]}}`))}, nil
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
			return nil, nil
		}
	}))
	token, err := api.tokens.Issue("admin", "", identity.UserType, identity.AdminScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/apis/iam.ebs/v1/users/alice", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK || calls != 1 || strings.Contains(response.Body.String(), "ebs:admin") {
		t.Fatalf("status=%d calls=%d body=%q", response.Code, calls, response.Body.String())
	}
}

func TestUserListOmitsAdminAccounts(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.Path {
		case "/apis/iam.ebs/v1/users/admin":
			body = `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`
		case "/apis/iam.ebs/v1/users":
			body = `{"items":[{"metadata":{"name":"admin"},"spec":{"scopes":["ebs:admin"]}},{"metadata":{"name":"alice"},"spec":{"scopes":["ebs:user"]}}]}`
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	token, err := api.tokens.Issue("admin", "", identity.UserType, identity.AdminScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/apis/iam.ebs/v1/users", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"name":"admin"`) || !strings.Contains(response.Body.String(), `"name":"alice"`) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}
