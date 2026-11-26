package middleware

import "net/http"

// routeKey matches an HTTP method and Gin route template, not a raw request path.
type routeKey struct {
	Method string
	Path   string
}

var unauthenticatedRoutes = map[routeKey]struct{}{
	{http.MethodGet, "/openapi.json"}:                   {},
	{http.MethodGet, "/auth/providers"}:                 {},
	{http.MethodGet, "/auth/oauth/:provider/login"}:     {},
	{http.MethodPost, "/auth/register"}:                 {},
	{http.MethodPost, "/auth/login"}:                    {},
	{http.MethodPost, "/auth/machine-token"}:            {},
	{http.MethodPost, "/auth/oauth/:provider/callback"}: {},
	{http.MethodPost, "/auth/oauth/register"}:           {},
}

var optionalAuthenticationRoutes = map[routeKey]struct{}{
	{http.MethodGet, "/apis/ebs/v1/projects"}:                 {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project"}:        {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/status"}: {},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/builds"}:              {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/builds/:name"}:        {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/builds/:name/status"}: {},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs"}:              {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs/:name"}:        {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs/:name/status"}: {},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/oldjobs"}:       {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/oldjobs/:name"}: {},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/snapshots"}:              {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/snapshots/:name"}:        {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/snapshots/:name/status"}: {},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildinfos"}:              {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildinfos/:name"}:        {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildinfos/:name/status"}: {},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildgraphs"}:              {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildgraphs/:name"}:        {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildgraphs/:name/status"}: {},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/rpmrepos"}:              {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/rpmrepos/:name"}:        {},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/rpmrepos/:name/status"}: {},
}

var watchRoutes = map[routeKey]struct{}{
	{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs"}: {},
	{http.MethodGet, "/apis/ebs/v1/runners/:runner/jobs"}:   {},
}

type authenticationMode uint8

const (
	authenticationRequired authenticationMode = iota
	authenticationOptional
	authenticationNone
)

func routeAuthentication(method, path string) authenticationMode {
	route := routeKey{Method: method, Path: path}
	if _, ok := unauthenticatedRoutes[route]; ok {
		return authenticationNone
	}
	if _, ok := optionalAuthenticationRoutes[route]; ok {
		return authenticationOptional
	}
	return authenticationRequired
}
