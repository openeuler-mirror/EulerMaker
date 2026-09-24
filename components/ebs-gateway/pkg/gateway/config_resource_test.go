package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigVisibility(t *testing.T) {
	for _, visibility := range []string{"Public", "OpsOnly"} {
		t.Run(visibility, func(t *testing.T) {
			calls := 0
			gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != apiPrefix+"/configs/example" {
					t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"apiVersion":"ebs/v1","kind":"Config","metadata":{"name":"example"},"spec":{"visibility":%q,"content":"hello"}}`, visibility)
			}), 100, 200)
			for _, tc := range []struct {
				name   string
				claims *jwtClaims
				want   int
			}{
				{"anonymous", nil, map[bool]int{true: 200, false: 403}[visibility == "Public"]},
				{"user", ptrClaims(userClaims("alice")), map[bool]int{true: 200, false: 403}[visibility == "Public"]},
				{"ops", ptrClaims(opsClaims()), 200},
				{"admin", ptrClaims(adminClaims()), 200},
				{"system", ptrClaims(systemClaims()), 200},
			} {
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					var req *http.Request
					if tc.claims == nil {
						req = httptest.NewRequest(method, apiPrefix+"/configs/example", nil)
					} else {
						req = authenticatedRequest(t, method, apiPrefix+"/configs/example", nil, *tc.claims)
					}
					rec := httptest.NewRecorder()
					gw.ServeHTTP(rec, req)
					if rec.Code != tc.want {
						t.Fatalf("%s %s: status=%d want=%d: %s", tc.name, method, rec.Code, tc.want, rec.Body.String())
					}
					if method == http.MethodHead && rec.Body.Len() != 0 {
						t.Fatal("HEAD returned a body")
					}
				}
			}
			if calls != 10 {
				t.Fatalf("upstream GET calls=%d, want 10", calls)
			}
		})
	}
}

func TestConfigOperations(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }), 100, 200)
	for _, tc := range []struct {
		method, path string
		claims       *jwtClaims
		allowed      bool
	}{
		{http.MethodGet, "/configs", nil, false},
		{http.MethodGet, "/configs", ptrClaims(userClaims("alice")), false},
		{http.MethodGet, "/configs", ptrClaims(opsClaims()), true},
		{http.MethodPost, "/configs", ptrClaims(userClaims("alice")), false},
		{http.MethodPost, "/configs", ptrClaims(opsClaims()), true},
		{http.MethodPut, "/configs/example", ptrClaims(opsClaims()), true},
		{http.MethodPatch, "/configs/example", ptrClaims(opsClaims()), true},
		{http.MethodDelete, "/configs/build-target", ptrClaims(systemClaims()), false},
		{http.MethodDelete, "/configs/build-resource", ptrClaims(opsClaims()), false},
		{http.MethodGet, "/projects/p/configs/example", ptrClaims(adminClaims()), false},
	} {
		var req *http.Request
		if tc.claims == nil {
			req = httptest.NewRequest(tc.method, apiPrefix+tc.path, strings.NewReader("{}"))
		} else {
			req = authenticatedRequest(t, tc.method, apiPrefix+tc.path, strings.NewReader("{}"), *tc.claims)
		}
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if (rec.Code == http.StatusOK) != tc.allowed {
			t.Fatalf("%s %s: status=%d", tc.method, tc.path, rec.Code)
		}
	}
}

func ptrClaims(c jwtClaims) *jwtClaims { return &c }
