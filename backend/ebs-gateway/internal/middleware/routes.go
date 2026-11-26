package middleware

import "net/http"

// routeKey matches an HTTP method and Gin route template, not a raw request path.
type routeKey struct {
	Method string
	Path   string
}

var watchRoutes = map[routeKey]struct{}{
	{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs"}: {},
	{http.MethodGet, "/apis/ebs/v1/runners/:runner/jobs"}:   {},
}
