package gateway

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestResourceAuthorizationHandled(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, scope string
		handled, denied           bool
	}{
		{"ordinary project falls through", "PUT", "/projects/p", "ebs:user", false, false},
		{"admin project falls through", "PUT", "/projects/p", "ebs:admin", false, false},
		{"system job status falls through", "PUT", "/projects/p/jobs/j/status", "ebs:system", false, false},
		{"admin job status denied", "PUT", "/projects/p/jobs/j/status", "ebs:admin", true, true},
		{"system config deletion denied", "DELETE", "/configs/build-resource", "ebs:system", true, true},
		{"ops config update", "PUT", "/configs/build-resource", "ebs:ops", true, false},
		{"project-scoped config denied", "GET", "/projects/p/configs/build-resource", "ebs:admin", true, true},
		{"admin script deletion denied", "DELETE", "/scripts/rpmbuild", "ebs:admin", true, true},
		{"ops script write", "PUT", "/scripts/rpmbuild", "ebs:ops", true, false},
		{"user script write denied", "PUT", "/scripts/rpmbuild", "ebs:user", true, true},
		{"ops runner management", "PATCH", "/runners/r/status", "ebs:ops", true, false},
		{"user runner management denied", "PATCH", "/runners/r/status", "ebs:user", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, apiPrefix+tc.path, nil)
			_, handled, err := (&Gateway{}).authorizeResource(context.Background(), r, Identity{Scopes: []string{tc.scope}}, parseRoute(r.URL.Path))
			if handled != tc.handled || (err != nil) != tc.denied {
				t.Fatalf("handled=%v err=%v; want handled=%v denied=%v", handled, err, tc.handled, tc.denied)
			}
		})
	}
}
