package gateway

import (
	"ebs-gateway/internal/middleware"
	"net/http"
)

// routeRules returns an independent set of business route access rules.
func routeRules() map[middleware.RouteKey]middleware.RoutePolicy {
	return map[middleware.RouteKey]middleware.RoutePolicy{
		{Method: http.MethodGet, Path: "/openapi.json"}: {Authentication: middleware.AuthenticationNone, Identities: middleware.AllowAnonymous},

		{Method: http.MethodGet, Path: "/auth/providers"}:                 {Authentication: middleware.AuthenticationNone, Identities: middleware.AllowAnonymous},
		{Method: http.MethodGet, Path: "/auth/oauth/:provider/login"}:     {Authentication: middleware.AuthenticationNone, Identities: middleware.AllowAnonymous},
		{Method: http.MethodPost, Path: "/auth/register"}:                 {Authentication: middleware.AuthenticationNone, Identities: middleware.AllowAnonymous},
		{Method: http.MethodPost, Path: "/auth/login"}:                    {Authentication: middleware.AuthenticationNone, Identities: middleware.AllowAnonymous},
		{Method: http.MethodPost, Path: "/auth/machine-token"}:            {Authentication: middleware.AuthenticationNone, Identities: middleware.AllowAnonymous},
		{Method: http.MethodPost, Path: "/auth/oauth/:provider/callback"}: {Authentication: middleware.AuthenticationNone, Identities: middleware.AllowAnonymous},
		{Method: http.MethodPost, Path: "/auth/oauth/register"}:           {Authentication: middleware.AuthenticationNone, Identities: middleware.AllowAnonymous},
		{Method: http.MethodPost, Path: "/auth/check"}:                    {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAuthenticated},
		{Method: http.MethodPut, Path: "/auth/users/:name/password"}:      {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers},
		{Method: http.MethodPost, Path: "/auth/machineaccounts"}:          {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},

		{Method: http.MethodGet, Path: "/apis/iam.ebs/v1/users"}:          {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},
		{Method: http.MethodGet, Path: "/apis/iam.ebs/v1/users/:name"}:    {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},
		{Method: http.MethodPut, Path: "/apis/iam.ebs/v1/users/:name"}:    {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},
		{Method: http.MethodPatch, Path: "/apis/iam.ebs/v1/users/:name"}:  {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},
		{Method: http.MethodDelete, Path: "/apis/iam.ebs/v1/users/:name"}: {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},

		{Method: http.MethodGet, Path: "/apis/iam.ebs/v1/machineaccounts"}:          {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},
		{Method: http.MethodGet, Path: "/apis/iam.ebs/v1/machineaccounts/:name"}:    {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},
		{Method: http.MethodPut, Path: "/apis/iam.ebs/v1/machineaccounts/:name"}:    {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},
		{Method: http.MethodPatch, Path: "/apis/iam.ebs/v1/machineaccounts/:name"}:  {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},
		{Method: http.MethodDelete, Path: "/apis/iam.ebs/v1/machineaccounts/:name"}: {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowAdmin},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/build-targets"}: {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/service-endpoints"}: {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowRunner},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/configs"}:          {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodPost, Path: "/apis/ebs/v1/configs"}:         {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/configs/:name"}:    {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodPut, Path: "/apis/ebs/v1/configs/:name"}:    {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodPatch, Path: "/apis/ebs/v1/configs/:name"}:  {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodDelete, Path: "/apis/ebs/v1/configs/:name"}: {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/scripts"}:          {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers | middleware.AllowRunner},
		{Method: http.MethodPost, Path: "/apis/ebs/v1/scripts"}:         {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/scripts/:name"}:    {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers | middleware.AllowRunner},
		{Method: http.MethodPut, Path: "/apis/ebs/v1/scripts/:name"}:    {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodPatch, Path: "/apis/ebs/v1/scripts/:name"}:  {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodDelete, Path: "/apis/ebs/v1/scripts/:name"}: {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects"}:            {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers, Options: middleware.RouteOptions{ListLimit: 100}},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project"}:   {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},
		{Method: http.MethodPost, Path: "/apis/ebs/v1/projects"}:           {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers},
		{Method: http.MethodPut, Path: "/apis/ebs/v1/projects/:project"}:   {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers | middleware.AllowService},
		{Method: http.MethodPatch, Path: "/apis/ebs/v1/projects/:project"}: {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers | middleware.AllowService},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/runners"}:                  {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodPost, Path: "/apis/ebs/v1/runners"}:                 {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowRunner},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/runners/:runner"}:          {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowRunner | middleware.AllowPrivileged},
		{Method: http.MethodPut, Path: "/apis/ebs/v1/runners/:runner"}:          {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowRunner | middleware.AllowPrivileged},
		{Method: http.MethodPatch, Path: "/apis/ebs/v1/runners/:runner"}:        {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowRunner | middleware.AllowPrivileged},
		{Method: http.MethodDelete, Path: "/apis/ebs/v1/runners/:runner"}:       {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/runners/:runner/status"}:   {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowPrivileged},
		{Method: http.MethodPut, Path: "/apis/ebs/v1/runners/:runner/status"}:   {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowRunner | middleware.AllowPrivileged},
		{Method: http.MethodPatch, Path: "/apis/ebs/v1/runners/:runner/status"}: {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowRunner | middleware.AllowPrivileged},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/runners/:runner/jobs"}:     {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowRunner, Options: middleware.RouteOptions{Watch: true}},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/builds"}:              {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers, Options: middleware.RouteOptions{ListLimit: 100}},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/builds/:name"}:        {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/builds/:name/status"}: {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},
		{Method: http.MethodPost, Path: "/apis/ebs/v1/projects/:project/builds"}:             {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers | middleware.AllowService},
		{Method: http.MethodDelete, Path: "/apis/ebs/v1/projects/:project/builds/:name"}:     {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers},
		{Method: http.MethodPost, Path: "/apis/ebs/v1/projects/:project/builds/:name/abort"}: {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers | middleware.AllowService},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/jobs"}:                {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers, Options: middleware.RouteOptions{Watch: true, ListLimit: 100}},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/jobs/:name"}:          {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers | middleware.AllowRunner},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/jobs/:name/status"}:   {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},
		{Method: http.MethodPatch, Path: "/apis/ebs/v1/projects/:project/jobs/:name/status"}: {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowRunner},
		{Method: http.MethodPost, Path: "/apis/ebs/v1/projects/:project/jobs/:name/abort"}:   {Authentication: middleware.AuthenticationRequired, Identities: middleware.AllowUsers},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/oldjobs"}:       {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers, Options: middleware.RouteOptions{ListLimit: 100}},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/oldjobs/:name"}: {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/snapshots"}:              {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers, Options: middleware.RouteOptions{ListLimit: 100}},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/snapshots/:name"}:        {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/snapshots/:name/status"}: {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/buildinfos"}:              {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers, Options: middleware.RouteOptions{ListLimit: 100}},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/buildinfos/:name"}:        {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/buildinfos/:name/status"}: {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/buildgraphs"}:              {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers, Options: middleware.RouteOptions{ListLimit: 100}},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/buildgraphs/:name"}:        {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/buildgraphs/:name/status"}: {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},

		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/rpmrepos"}:              {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers, Options: middleware.RouteOptions{ListLimit: 100}},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/rpmrepos/:name"}:        {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},
		{Method: http.MethodGet, Path: "/apis/ebs/v1/projects/:project/rpmrepos/:name/status"}: {Authentication: middleware.AuthenticationOptional, Identities: middleware.AllowAnonymous | middleware.AllowUsers},
	}

}
