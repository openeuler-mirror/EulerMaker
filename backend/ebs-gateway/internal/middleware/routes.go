package middleware

import "maps"

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
	Watch          bool
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
