package middleware

import (
	"ebs-gateway/internal/policy"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
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

type RouteAccess struct {
	Authentication AuthenticationMode
	Authorization  policy.Rule
}

type RouteOptions struct {
	Watch     bool
	ListLimit int
}

type RoutePolicy struct {
	Access  RouteAccess
	Options RouteOptions
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
		return rule.Access.Authentication
	}
	return AuthenticationRequired
}

// ValidateRoute checks authentication, authorization and request options during registration.
func ValidateRoute(key RouteKey, access RouteAccess, options RouteOptions) error {
	if err := access.Authorization.Validate(); err != nil {
		return err
	}
	switch access.Authentication {
	case AuthenticationRequired:
		if access.Authorization.Identities&policy.AllowAnonymous != 0 {
			return fmt.Errorf("required authentication cannot allow anonymous access")
		}
	case AuthenticationOptional:
		if access.Authorization.Identities&policy.AllowAnonymous == 0 {
			return fmt.Errorf("optional authentication requires anonymous access")
		}
	case AuthenticationNone:
		if access.Authorization.Identities != policy.AllowAnonymous {
			return fmt.Errorf("unauthenticated routes must allow only anonymous access")
		}
	default:
		return fmt.Errorf("unknown authentication mode")
	}
	if options.ListLimit < 0 || (options.ListLimit > 0 && key.Method != http.MethodGet) {
		return fmt.Errorf("list limit must be nonnegative and only configured for GET")
	}
	if options.Watch && (key.Method != http.MethodGet || access.Authentication == AuthenticationNone) {
		return fmt.Errorf("Watch requires GET with authentication")
	}
	if !options.Watch && access.Authorization.WatchAccess != policy.ObjectAccessNone {
		return fmt.Errorf("Watch access requires Watch support")
	}
	segments := strings.Split(key.Path, "/")
	for _, object := range []policy.ObjectAccess{access.Authorization.ObjectAccess, access.Authorization.WatchAccess} {
		switch object {
		case policy.ProjectOwner, policy.ProjectOwnerUpdate, policy.ProjectMember:
			if !slices.Contains(segments, ":project") {
				return fmt.Errorf("project ownership requires a :project parameter")
			}
		case policy.RunnerSelf:
			if !slices.Contains(segments, ":runner") {
				return fmt.Errorf("Runner ownership requires a :runner parameter")
			}
		case policy.AssignedJob:
			if !slices.Contains(segments, ":project") || !slices.Contains(segments, ":name") {
				return fmt.Errorf("Job assignment requires :project and :name parameters")
			}
		}
	}
	if access.Authorization.ServicePermission != "" && !slices.Contains(segments, ":project") {
		return fmt.Errorf("service permission requires a :project parameter")
	}
	return nil
}
