package handler

import (
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/policy"
)

func validateResourceRoute(c *gin.Context, route policy.Route) bool {
	switch route.Resource {
	case "projects":
		return validateProjectRoute(c, route)
	case "runners":
		return validateRunnerRoute(c, route)
	case "builds":
		return validateBuildRoute(c, route)
	case "jobs":
		return validateJobRoute(c, route)
	case "configs":
		return validateConfigRoute(c, route)
	case "scripts":
		return validateScriptRoute(c, route)
	case "snapshots", "buildinfos", "buildgraphs", "rpmrepos":
		return validateReadOnlyRoute(c, route)
	case "oldjobs":
		return validateOldJobRoute(c, route)
	default:
		return validateRouteMethods(c, route, false)
	}
}

func validateProjectRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Name == "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPost)
	case route.Name != "" && (route.Subresource == "" || route.Subresource == "status"):
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch)
	default:
		return validateRouteMethods(c, route, false)
	}
}

func validateRunnerRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Name == "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPost)
	case route.Name != "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete)
	case route.Name != "" && route.Subresource == "status":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch)
	case route.Name != "" && route.Subresource == "jobs":
		return validateRouteMethods(c, route, route.Project == "", http.MethodGet)
	default:
		return validateRouteMethods(c, route, false)
	}
}

func validateBuildRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Name == "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPost)
	case route.Name != "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete)
	case route.Name != "" && route.Subresource == "status":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch)
	case route.Name != "" && route.Subresource == "abort":
		return validateRouteMethods(c, route, false, http.MethodPost)
	default:
		return validateRouteMethods(c, route, false)
	}
}

func validateJobRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Name == "" && route.Subresource == "":
		return validateRouteMethods(c, route, route.Project != "" && route.Method == http.MethodGet,
			http.MethodGet, http.MethodHead, http.MethodPost)
	case route.Name != "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete)
	case route.Name != "" && route.Subresource == "status":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch)
	case route.Name != "" && route.Subresource == "abort":
		return validateRouteMethods(c, route, false, http.MethodPost)
	default:
		return validateRouteMethods(c, route, false)
	}
}

func validateConfigRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Subresource != "":
		return validateRouteMethods(c, route, false)
	case route.Name == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPost)
	default:
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete)
	}
}

func validateScriptRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Subresource != "":
		return validateRouteMethods(c, route, false)
	case route.Name == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodPost)
	default:
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete)
	}
}

func validateReadOnlyRoute(c *gin.Context, route policy.Route) bool {
	if route.Subresource == "" || (route.Name != "" && route.Subresource == "status") {
		return validateRouteMethods(c, route, false, http.MethodGet)
	}
	return validateRouteMethods(c, route, false)
}

func validateOldJobRoute(c *gin.Context, route policy.Route) bool {
	if route.Subresource == "" {
		return validateRouteMethods(c, route, false, http.MethodGet)
	}
	return validateRouteMethods(c, route, false)
}

func validateRouteMethods(c *gin.Context, route policy.Route, watchAllowed bool, methods ...string) bool {
	if !slices.Contains(methods, route.Method) {
		c.Header("Allow", strings.Join(methods, ", "))
		c.AbortWithStatus(http.StatusMethodNotAllowed)
		return false
	}
	if policy.HasWatch(c.Request.URL.Query()) && !watchAllowed {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "watch is not supported for this route"})
		return false
	}
	return true
}
