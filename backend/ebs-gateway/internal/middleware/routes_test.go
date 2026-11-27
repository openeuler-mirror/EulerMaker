package middleware

import (
	"ebs-gateway/internal/policy"
	"net/http"
	"testing"
)

func TestRoutePoliciesIsolation(t *testing.T) {
	key := RouteKey{http.MethodGet, "/items/:name"}
	rules := map[RouteKey]RoutePolicy{key: {Access: RouteAccess{Authentication: AuthenticationOptional, Authorization: policy.Rule{Identities: policy.AllowAnonymous | policy.AllowUsers}}}}
	first := NewRoutePolicies(rules)
	rules[key] = RoutePolicy{Access: RouteAccess{Authentication: AuthenticationNone, Authorization: policy.Rule{Identities: policy.AllowAnonymous}}}
	second := NewRoutePolicies(rules)
	delete(rules, key)
	if first.authentication(key.Method, key.Path) != AuthenticationOptional ||
		second.authentication(key.Method, key.Path) != AuthenticationNone {
		t.Fatal("route policies share mutable input")
	}
	for _, route := range []RouteKey{
		{http.MethodPost, key.Path},
		{http.MethodGet, "/items/one"},
		{http.MethodGet, "/missing"},
		{http.MethodGet, ""},
	} {
		if got := first.authentication(route.Method, route.Path); got != AuthenticationRequired {
			t.Errorf("%+v: authentication=%v, want required", route, got)
		}
	}
	if got := NewRoutePolicies(nil).authentication(key.Method, key.Path); got != AuthenticationRequired {
		t.Fatalf("unconfigured route authentication=%v, want required", got)
	}
}
