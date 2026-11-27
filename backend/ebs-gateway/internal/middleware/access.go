package middleware

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/policy"
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

// AdmitIdentity checks allowed identities without business resource authorization.
func AdmitIdentity(allowed policy.RouteIdentities) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !allowed.Allows(Principal(c)) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "identity is not allowed for this route"})
			return
		}
		c.Next()
	}
}

// Authorize adapts the policy decision to HTTP responses, audit fields and request-local data.
func Authorize(routes *RoutePolicies, authorizer *policy.Authorizer) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if path == "" {
			c.Next()
			return
		}
		who := Principal(c)
		rule := routes.rules[RouteKey{Method: c.Request.Method, Path: path}].Access.Authorization
		request := policy.Request{
			Method:  c.Request.Method,
			Project: c.Param("project"),
			Runner:  c.Param("runner"),
			Path:    c.Request.URL.Path,
			Watch:   policy.HasWatch(c.Request.URL.Query()),
		}
		if who.Type == identity.ServiceType {
			c.Set("serviceProject", request.Project)
			c.Set("servicePermission", string(rule.ServicePermission))
			c.Set("serviceAuthorization", "denied")
		}
		result, err := authorizer.Authorize(c.Request.Context(), who, request, rule)
		if err != nil {
			status := http.StatusForbidden
			if classified, ok := err.(*policy.Error); ok {
				status = classified.Status
			}
			c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
			return
		}
		if who.Type == identity.ServiceType {
			c.Set("serviceAuthorization", "allowed")
		}
		if result.UpdateObject != nil {
			c.Set(updateObjectKey, result.UpdateObject)
		}
		c.Next()
	}
}

const updateObjectKey = "ebs-gateway.authorizedUpdateObject"

// UpdateObject returns the full object already read during update authorization.
func UpdateObject(c *gin.Context) []byte {
	value, _ := c.Get(updateObjectKey)
	raw, _ := value.([]byte)
	return raw
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
