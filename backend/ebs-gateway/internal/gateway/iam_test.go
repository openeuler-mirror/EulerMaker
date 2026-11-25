package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/upstream"
)

func TestNamedUserProtection(t *testing.T) {
	const ordinary = `{"spec":{"scopes":["ebs:user"]}}`
	for _, tc := range []struct {
		name, object string
		status       int
	}{
		{"ordinary", ordinary, http.StatusOK},
		{"admin", `{"spec":{"scopes":["ebs:admin"]}}`, http.StatusForbidden},
		{"oversized", ordinary + strings.Repeat(" ", 64), http.StatusBadGateway},
	} {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				var calls []string
				client, err := upstream.New("http://iam", roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls = append(calls, r.Method)
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.object))}, nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				api := &Handler{upstream: client, bodyLimit: 64}
				router := gin.New()
				router.Handle(method, "/users/:name", api.users)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(method, "/users/example", nil))
				wantCalls := "GET"
				if method == http.MethodDelete && tc.status == http.StatusOK {
					wantCalls = "GET,DELETE"
				}
				if response.Code != tc.status || strings.Join(calls, ",") != wantCalls {
					t.Fatalf("status=%d upstream=%v; want status=%d upstream=%s", response.Code, calls, tc.status, wantCalls)
				}
				if tc.status == http.StatusOK && method == http.MethodGet && response.Body.String() != tc.object {
					t.Fatalf("unexpected response body %q", response.Body)
				}
			})
		}
	}
}
