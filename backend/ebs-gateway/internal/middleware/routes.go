package middleware

import (
	"fmt"
	"maps"
	"net/http"
)

// RouteKey matches an HTTP method and Gin route template, not a raw request path.
type RouteKey struct {
	Method string
	Path   string
}

type AuthenticationMode uint8

const (
	AuthenticationRequired AuthenticationMode = iota
	AuthenticationOptional
	AuthenticationNone
)

type RouteIdentities uint8

const (
	AllowAnonymous RouteIdentities = 1 << iota
	AllowUser
	AllowOps
	AllowAdmin
	AllowRunner
	AllowService

	AllowPrivileged    = AllowOps | AllowAdmin
	AllowUsers         = AllowUser | AllowOps | AllowAdmin
	AllowAuthenticated = AllowUsers | AllowRunner | AllowService
)

type RoutePolicy struct {
	Authentication AuthenticationMode
	Identities     RouteIdentities
	Options        RouteOptions
}

type RouteOptions struct {
	Watch     bool
	ListLimit int
}

// RoutePolicies holds an immutable, router-local copy of the access rules.
type RoutePolicies struct {
	rules map[RouteKey]RoutePolicy
}

func NewRoutePolicies(rules map[RouteKey]RoutePolicy) *RoutePolicies {
	return &RoutePolicies{rules: maps.Clone(rules)}
}

func (r *RoutePolicies) authentication(method, path string) AuthenticationMode {
	if rule, ok := r.rules[RouteKey{Method: method, Path: path}]; ok {
		return rule.Authentication
	}
	return AuthenticationRequired
}

// ValidateRoute rejects inconsistent authentication, identity and query declarations.
func ValidateRoute(key RouteKey, rule RoutePolicy) error {
	if rule.Identities == 0 || rule.Identities & ^(AllowAnonymous|AllowAuthenticated) != 0 {
		return fmt.Errorf("allowed identities are empty or unknown")
	}
	switch rule.Authentication {
	case AuthenticationRequired:
		if rule.Identities&AllowAnonymous != 0 {
			return fmt.Errorf("required authentication cannot allow anonymous access")
		}
	case AuthenticationOptional:
		if rule.Identities&AllowAnonymous == 0 {
			return fmt.Errorf("optional authentication requires anonymous access")
		}
	case AuthenticationNone:
		if rule.Identities != AllowAnonymous {
			return fmt.Errorf("unauthenticated routes must allow only anonymous access")
		}
	default:
		return fmt.Errorf("unknown authentication mode")
	}
	if rule.Options.ListLimit < 0 || (rule.Options.ListLimit > 0 && key.Method != http.MethodGet) {
		return fmt.Errorf("list limit must be nonnegative and only configured for GET")
	}
	if rule.Options.Watch && (key.Method != http.MethodGet || rule.Authentication == AuthenticationNone) {
		return fmt.Errorf("Watch requires GET with authentication")
	}
	return nil
}
