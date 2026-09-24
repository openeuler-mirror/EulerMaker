package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildUpdatesAreDeniedThroughGateway(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		for _, path := range []string{
			"/apis/ebs/v1/projects/project-a/builds",
			"/apis/ebs/v1/projects/project-a/builds/build-a",
			"/apis/ebs/v1/projects/project-a/builds/build-a/status",
		} {
			for _, scopes := range [][]string{{"ebs:user"}, {"ebs:ops"}, {"ebs:admin"}, {"ebs:system"}} {
				req := httptest.NewRequest(method, path, nil)
				identity := Identity{Scopes: scopes}
				_, handled, err := (&Gateway{}).authorizeResource(context.Background(), req, identity, parseRoute(req.URL.Path))
				if !handled || err == nil {
					t.Fatalf("method=%s path=%s scopes=%v handled=%t err=%v", method, path, scopes, handled, err)
				}
			}
		}
	}
}

func TestBuildCreateAndAbortAreNotBlockedByUpdateRule(t *testing.T) {
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/apis/ebs/v1/projects/project-a/builds"},
		{http.MethodPost, "/apis/ebs/v1/projects/project-a/builds/build-a/abort"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		_, handled, err := (&Gateway{}).authorizeResource(context.Background(), req, Identity{Scopes: []string{"ebs:system"}}, parseRoute(req.URL.Path))
		if handled || err != nil {
			t.Fatalf("method=%s path=%s handled=%t err=%v", tc.method, tc.path, handled, err)
		}
	}
}
