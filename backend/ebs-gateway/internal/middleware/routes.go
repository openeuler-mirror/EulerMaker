package middleware

import "net/http"

// routeKey matches an HTTP method and Gin route template, not a raw request path.
type routeKey struct {
	Method string
	Path   string
}

type authenticationMode uint8

const (
	authenticationRequired authenticationMode = iota
	authenticationOptional
	authenticationNone
)

type routeIdentities uint8

const (
	allowAnonymous routeIdentities = 1 << iota
	allowUser
	allowOps
	allowAdmin
	allowRunner
	allowService

	allowPrivileged    = allowOps | allowAdmin
	allowUsers         = allowUser | allowOps | allowAdmin
	allowAuthenticated = allowUsers | allowRunner | allowService
)

type routePolicy struct {
	Authentication authenticationMode
	Identities     routeIdentities
	Watch          bool
}

// routePolicies declares route authentication, identity admission and Watch support.
// Admission does not replace object ownership, service grants or writable-field checks.
var routePolicies = map[routeKey]routePolicy{
	{http.MethodGet, "/openapi.json"}: {authenticationNone, allowAnonymous, false},

	{http.MethodGet, "/auth/providers"}:                 {authenticationNone, allowAnonymous, false},
	{http.MethodGet, "/auth/oauth/:provider/login"}:     {authenticationNone, allowAnonymous, false},
	{http.MethodPost, "/auth/register"}:                 {authenticationNone, allowAnonymous, false},
	{http.MethodPost, "/auth/login"}:                    {authenticationNone, allowAnonymous, false},
	{http.MethodPost, "/auth/machine-token"}:            {authenticationNone, allowAnonymous, false},
	{http.MethodPost, "/auth/oauth/:provider/callback"}: {authenticationNone, allowAnonymous, false},
	{http.MethodPost, "/auth/oauth/register"}:           {authenticationNone, allowAnonymous, false},
	{http.MethodPost, "/auth/check"}:                    {authenticationRequired, allowAuthenticated, false},
	{http.MethodPut, "/auth/users/:name/password"}:      {authenticationRequired, allowUsers, false},
	{http.MethodPost, "/auth/machineaccounts"}:          {authenticationRequired, allowAdmin, false},

	{http.MethodGet, "/apis/iam.ebs/v1/users"}:          {authenticationRequired, allowAdmin, false},
	{http.MethodGet, "/apis/iam.ebs/v1/users/:name"}:    {authenticationRequired, allowAdmin, false},
	{http.MethodPut, "/apis/iam.ebs/v1/users/:name"}:    {authenticationRequired, allowAdmin, false},
	{http.MethodPatch, "/apis/iam.ebs/v1/users/:name"}:  {authenticationRequired, allowAdmin, false},
	{http.MethodDelete, "/apis/iam.ebs/v1/users/:name"}: {authenticationRequired, allowAdmin, false},

	{http.MethodGet, "/apis/iam.ebs/v1/machineaccounts"}:          {authenticationRequired, allowAdmin, false},
	{http.MethodGet, "/apis/iam.ebs/v1/machineaccounts/:name"}:    {authenticationRequired, allowAdmin, false},
	{http.MethodPut, "/apis/iam.ebs/v1/machineaccounts/:name"}:    {authenticationRequired, allowAdmin, false},
	{http.MethodPatch, "/apis/iam.ebs/v1/machineaccounts/:name"}:  {authenticationRequired, allowAdmin, false},
	{http.MethodDelete, "/apis/iam.ebs/v1/machineaccounts/:name"}: {authenticationRequired, allowAdmin, false},

	{http.MethodGet, "/apis/ebs/v1/build-targets"}: {authenticationRequired, allowUsers, false},

	{http.MethodGet, "/apis/ebs/v1/service-endpoints"}: {authenticationRequired, allowRunner, false},

	{http.MethodGet, "/apis/ebs/v1/configs"}:          {authenticationRequired, allowPrivileged, false},
	{http.MethodPost, "/apis/ebs/v1/configs"}:         {authenticationRequired, allowPrivileged, false},
	{http.MethodGet, "/apis/ebs/v1/configs/:name"}:    {authenticationRequired, allowPrivileged, false},
	{http.MethodPut, "/apis/ebs/v1/configs/:name"}:    {authenticationRequired, allowPrivileged, false},
	{http.MethodPatch, "/apis/ebs/v1/configs/:name"}:  {authenticationRequired, allowPrivileged, false},
	{http.MethodDelete, "/apis/ebs/v1/configs/:name"}: {authenticationRequired, allowPrivileged, false},

	{http.MethodGet, "/apis/ebs/v1/scripts"}:          {authenticationRequired, allowAuthenticated, false},
	{http.MethodPost, "/apis/ebs/v1/scripts"}:         {authenticationRequired, allowPrivileged, false},
	{http.MethodGet, "/apis/ebs/v1/scripts/:name"}:    {authenticationRequired, allowAuthenticated, false},
	{http.MethodPut, "/apis/ebs/v1/scripts/:name"}:    {authenticationRequired, allowPrivileged, false},
	{http.MethodPatch, "/apis/ebs/v1/scripts/:name"}:  {authenticationRequired, allowPrivileged, false},
	{http.MethodDelete, "/apis/ebs/v1/scripts/:name"}: {authenticationRequired, allowPrivileged, false},

	{http.MethodGet, "/apis/ebs/v1/projects"}:                   {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project"}:          {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/status"}:   {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodPost, "/apis/ebs/v1/projects"}:                  {authenticationRequired, allowUsers, false},
	{http.MethodPut, "/apis/ebs/v1/projects/:project"}:          {authenticationRequired, allowUsers | allowService, false},
	{http.MethodPut, "/apis/ebs/v1/projects/:project/status"}:   {authenticationRequired, allowUser | allowOps, false},
	{http.MethodPatch, "/apis/ebs/v1/projects/:project"}:        {authenticationRequired, allowUsers | allowService, false},
	{http.MethodPatch, "/apis/ebs/v1/projects/:project/status"}: {authenticationRequired, allowUser | allowOps, false},

	{http.MethodGet, "/apis/ebs/v1/runners"}:                  {authenticationRequired, allowPrivileged, false},
	{http.MethodPost, "/apis/ebs/v1/runners"}:                 {authenticationRequired, allowRunner, false},
	{http.MethodGet, "/apis/ebs/v1/runners/:runner"}:          {authenticationRequired, allowRunner | allowPrivileged, false},
	{http.MethodPut, "/apis/ebs/v1/runners/:runner"}:          {authenticationRequired, allowRunner | allowPrivileged, false},
	{http.MethodPatch, "/apis/ebs/v1/runners/:runner"}:        {authenticationRequired, allowRunner | allowPrivileged, false},
	{http.MethodDelete, "/apis/ebs/v1/runners/:runner"}:       {authenticationRequired, allowPrivileged, false},
	{http.MethodGet, "/apis/ebs/v1/runners/:runner/status"}:   {authenticationRequired, allowPrivileged, false},
	{http.MethodPut, "/apis/ebs/v1/runners/:runner/status"}:   {authenticationRequired, allowRunner | allowPrivileged, false},
	{http.MethodPatch, "/apis/ebs/v1/runners/:runner/status"}: {authenticationRequired, allowRunner | allowPrivileged, false},
	{http.MethodGet, "/apis/ebs/v1/runners/:runner/jobs"}:     {authenticationRequired, allowRunner, true},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/builds"}:              {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/builds/:name"}:        {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/builds/:name/status"}: {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodPost, "/apis/ebs/v1/projects/:project/builds"}:             {authenticationRequired, allowUsers | allowService, false},
	{http.MethodDelete, "/apis/ebs/v1/projects/:project/builds/:name"}:     {authenticationRequired, allowUsers, false},
	{http.MethodPost, "/apis/ebs/v1/projects/:project/builds/:name/abort"}: {authenticationRequired, allowUsers | allowService, false},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs"}:                {authenticationOptional, allowAnonymous | allowUsers, true},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs/:name"}:          {authenticationOptional, allowAnonymous | allowUsers | allowRunner, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs/:name/status"}:   {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodPatch, "/apis/ebs/v1/projects/:project/jobs/:name/status"}: {authenticationRequired, allowRunner, false},
	{http.MethodPost, "/apis/ebs/v1/projects/:project/jobs/:name/abort"}:   {authenticationRequired, allowUsers, false},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/oldjobs"}:       {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/oldjobs/:name"}: {authenticationOptional, allowAnonymous | allowUsers, false},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/snapshots"}:              {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/snapshots/:name"}:        {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/snapshots/:name/status"}: {authenticationOptional, allowAnonymous | allowUsers, false},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildinfos"}:              {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildinfos/:name"}:        {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildinfos/:name/status"}: {authenticationOptional, allowAnonymous | allowUsers, false},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildgraphs"}:              {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildgraphs/:name"}:        {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/buildgraphs/:name/status"}: {authenticationOptional, allowAnonymous | allowUsers, false},

	{http.MethodGet, "/apis/ebs/v1/projects/:project/rpmrepos"}:              {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/rpmrepos/:name"}:        {authenticationOptional, allowAnonymous | allowUsers, false},
	{http.MethodGet, "/apis/ebs/v1/projects/:project/rpmrepos/:name/status"}: {authenticationOptional, allowAnonymous | allowUsers, false},
}

func routeAuthentication(method, path string) authenticationMode {
	if rule, ok := routePolicies[routeKey{Method: method, Path: path}]; ok {
		return rule.Authentication
	}
	return authenticationRequired
}

// HasRoutePolicy reports whether a registered business route has an explicit access policy.
func HasRoutePolicy(method, path string) bool {
	_, ok := routePolicies[routeKey{Method: method, Path: path}]
	return ok
}
