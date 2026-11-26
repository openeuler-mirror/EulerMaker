package middleware

import (
	"net/http"
	"testing"
)

func TestRouteAuthentication(t *testing.T) {
	for _, tc := range []struct {
		route routeKey
		mode  authenticationMode
	}{
		{routeKey{http.MethodPost, "/auth/login"}, authenticationNone},
		{routeKey{http.MethodGet, "/auth/login"}, authenticationRequired},
		{routeKey{http.MethodGet, "/apis/ebs/v1/projects/:project/buildinfos/:name/status"}, authenticationOptional},
		{routeKey{http.MethodPut, "/apis/ebs/v1/projects/:project/buildinfos/:name/status"}, authenticationRequired},
		{routeKey{http.MethodGet, "/apis/ebs/v1/projects/demo/buildinfos/build/status"}, authenticationRequired},
		{routeKey{http.MethodGet, "/apis/ebs/v1/new-resource"}, authenticationRequired},
		{routeKey{http.MethodGet, ""}, authenticationRequired},
	} {
		if got := routeAuthentication(tc.route.Method, tc.route.Path); got != tc.mode {
			t.Errorf("%s %s: mode=%v, want %v", tc.route.Method, tc.route.Path, got, tc.mode)
		}
	}
	for route, rule := range routePolicies {
		if rule.Identities == 0 {
			t.Errorf("route has no allowed identities: %+v", route)
		}
		if rule.Watch && (route.Method != http.MethodGet || rule.Authentication == authenticationNone) {
			t.Errorf("Watch must use GET and cannot bypass authentication: %+v", route)
		}
		if (rule.Authentication != authenticationRequired) != (rule.Identities&allowAnonymous != 0) {
			t.Errorf("anonymous admission and authentication disagree: %+v", route)
		}
	}
}
