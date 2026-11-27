package gateway

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	iamv1 "ebs-api/iam/v1"
	"ebs-gateway/internal/auth"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/oauth"
	"ebs-gateway/internal/openapi"
	"ebs-gateway/internal/policy"
)

type routeDefinition struct {
	Method         string
	Path           string
	Authentication middleware.AuthenticationMode
	Identities     middleware.RouteIdentities
	Options        middleware.RouteOptions
	Middlewares    []gin.HandlerFunc
	Handler        gin.HandlerFunc
}

func NewRouter(api *Handler, authHandler *auth.Handler, oauthHandler *oauth.Handler) *gin.Engine {
	router := gin.New()
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.RemoveExtraSlash = false
	_ = router.SetTrustedProxies(nil)
	// Register probes before the business middleware chain to avoid auditing or throttling them.
	registerHealthz(router)
	access := middleware.NewRoutePolicies(nil)
	router.Use(
		middleware.Audit(api.now),
		gin.Recovery(),
		middleware.PrepareQuery(access),
		middleware.Authenticate(api.auth, access),
		middleware.LimitIdentity(api.limits),
		middleware.AdmitIdentity(access),
	)
	rules := make(map[middleware.RouteKey]middleware.RoutePolicy)
	registerAuthRoutes(router, rules, authHandler)
	registerOAuthRoutes(router, rules, oauthHandler)
	registerForwardRoutes(router, rules, api)
	registerOtherRoutes(router, rules, api)
	// Publish the completed, immutable policy table before serving requests.
	*access = *middleware.NewRoutePolicies(rules)
	router.NoRoute(func(c *gin.Context) {
		c.Status(http.StatusNotFound)
	})
	return router
}

func registerHealthz(router *gin.Engine) {
	router.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok\n") })
}

func registerAuthRoutes(router *gin.Engine, rules map[middleware.RouteKey]middleware.RoutePolicy, authHandler *auth.Handler) {
	routes := []routeDefinition{
		{
			Method:         http.MethodPost,
			Path:           "/auth/check",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAuthenticated,
			Handler:        authHandler.CheckToken,
		},
		{
			Method:         http.MethodPost,
			Path:           "/auth/register",
			Authentication: middleware.AuthenticationNone,
			Handler:        authHandler.RegisterUser,
		},
		{
			Method:         http.MethodPost,
			Path:           "/auth/login",
			Authentication: middleware.AuthenticationNone,
			Handler:        authHandler.Login,
		},
		{
			Method:         http.MethodPost,
			Path:           "/auth/machine-token",
			Authentication: middleware.AuthenticationNone,
			Handler:        authHandler.MachineToken,
		},
		{
			Method:         http.MethodPut,
			Path:           "/auth/users/:name/password",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers,
			Handler:        authHandler.ChangePassword,
		},
		{
			Method:         http.MethodPost,
			Path:           "/auth/machineaccounts",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
			Handler:        authHandler.RegisterMachine,
		},
	}
	registerRoutes(router, rules, routes)
}

func registerOAuthRoutes(router *gin.Engine, rules map[middleware.RouteKey]middleware.RoutePolicy, oauthHandler *oauth.Handler) {
	routes := []routeDefinition{
		{
			Method:         http.MethodGet,
			Path:           "/auth/providers",
			Authentication: middleware.AuthenticationNone,
			Handler:        oauthHandler.Providers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/auth/oauth/:provider/login",
			Authentication: middleware.AuthenticationNone,
			Handler:        oauthHandler.Login,
		},
		{
			Method:         http.MethodPost,
			Path:           "/auth/oauth/:provider/callback",
			Authentication: middleware.AuthenticationNone,
			Handler:        oauthHandler.Callback,
		},
		{
			Method:         http.MethodPost,
			Path:           "/auth/oauth/register",
			Authentication: middleware.AuthenticationNone,
			Handler:        oauthHandler.Register,
		},
	}
	registerRoutes(router, rules, routes)
}

func registerForwardRoutes(router *gin.Engine, rules map[middleware.RouteKey]middleware.RoutePolicy, api *Handler) {
	routes := []routeDefinition{
		{
			Method:         http.MethodGet,
			Path:           "/apis/iam.ebs/v1/users",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/iam.ebs/v1/users/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
		},
		{
			Method:         http.MethodPut,
			Path:           "/apis/iam.ebs/v1/users/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
			Middlewares:    []gin.HandlerFunc{api.prepareUserUpdate},
		},
		{
			Method:         http.MethodPatch,
			Path:           "/apis/iam.ebs/v1/users/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
			Middlewares:    []gin.HandlerFunc{api.prepareUserUpdate},
		},
		{
			Method:         http.MethodDelete,
			Path:           "/apis/iam.ebs/v1/users/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
			Middlewares:    []gin.HandlerFunc{api.protectAdminUser},
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/iam.ebs/v1/machineaccounts",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/iam.ebs/v1/machineaccounts/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
		},
		{
			Method:         http.MethodPut,
			Path:           "/apis/iam.ebs/v1/machineaccounts/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
		},
		{
			Method:         http.MethodPatch,
			Path:           "/apis/iam.ebs/v1/machineaccounts/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
		},
		{
			Method:         http.MethodDelete,
			Path:           "/apis/iam.ebs/v1/machineaccounts/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowAdmin,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/configs",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodPost,
			Path:           "/apis/ebs/v1/configs",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/configs/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodPut,
			Path:           "/apis/ebs/v1/configs/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodPatch,
			Path:           "/apis/ebs/v1/configs/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodDelete,
			Path:           "/apis/ebs/v1/configs/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
			Middlewares:    []gin.HandlerFunc{rejectBuiltInConfigDeletion},
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/scripts",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers | middleware.AllowRunner,
		},
		{
			Method:         http.MethodPost,
			Path:           "/apis/ebs/v1/scripts",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/scripts/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers | middleware.AllowRunner,
		},
		{
			Method:         http.MethodPut,
			Path:           "/apis/ebs/v1/scripts/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodPatch,
			Path:           "/apis/ebs/v1/scripts/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodDelete,
			Path:           "/apis/ebs/v1/scripts/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects",
			Options:        middleware.RouteOptions{ListLimit: 100},
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodPost,
			Path:           "/apis/ebs/v1/projects",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers,
			Middlewares:    []gin.HandlerFunc{api.prepareProjectCreate},
		},
		{
			Method:         http.MethodPut,
			Path:           "/apis/ebs/v1/projects/:project",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers | middleware.AllowService,
			Middlewares: []gin.HandlerFunc{api.authorizeResource(policy.Rule{
				Identities:        policy.AllowUsers | policy.AllowService,
				ObjectAccess:      policy.ProjectOwnerUpdate,
				ServicePermission: iamv1.MachinePermissionProjectsUpdate,
			}), api.prepareProjectUpdate},
		},
		{
			Method:         http.MethodPatch,
			Path:           "/apis/ebs/v1/projects/:project",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers | middleware.AllowService,
			Middlewares: []gin.HandlerFunc{api.authorizeResource(policy.Rule{
				Identities:        policy.AllowUsers | policy.AllowService,
				ObjectAccess:      policy.ProjectOwnerUpdate,
				ServicePermission: iamv1.MachinePermissionProjectsUpdate,
			}), api.prepareProjectUpdate},
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/runners",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodPost,
			Path:           "/apis/ebs/v1/runners",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowRunner,
			Middlewares:    []gin.HandlerFunc{api.prepareRunnerCreate},
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/runners/:runner",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowRunner | middleware.AllowPrivileged,
			Middlewares:    []gin.HandlerFunc{api.runnerReadGuard},
		},
		{
			Method:         http.MethodPut,
			Path:           "/apis/ebs/v1/runners/:runner",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowRunner | middleware.AllowPrivileged,
			Middlewares:    []gin.HandlerFunc{api.runnerUpdateGuard},
		},
		{
			Method:         http.MethodPatch,
			Path:           "/apis/ebs/v1/runners/:runner",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowRunner | middleware.AllowPrivileged,
			Middlewares:    []gin.HandlerFunc{api.runnerUpdateGuard},
		},
		{
			Method:         http.MethodDelete,
			Path:           "/apis/ebs/v1/runners/:runner",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/runners/:runner/status",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowPrivileged,
		},
		{
			Method:         http.MethodPut,
			Path:           "/apis/ebs/v1/runners/:runner/status",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowRunner | middleware.AllowPrivileged,
			Middlewares:    []gin.HandlerFunc{api.runnerStatusUpdateGuard},
		},
		{
			Method:         http.MethodPatch,
			Path:           "/apis/ebs/v1/runners/:runner/status",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowRunner | middleware.AllowPrivileged,
			Middlewares:    []gin.HandlerFunc{api.runnerStatusUpdateGuard},
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/runners/:runner/jobs",
			Options:        middleware.RouteOptions{Watch: true},
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowRunner,
			Middlewares:    []gin.HandlerFunc{api.prepareRunnerJobs},
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/builds",
			Options:        middleware.RouteOptions{ListLimit: 100},
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/builds/:name",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/builds/:name/status",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodPost,
			Path:           "/apis/ebs/v1/projects/:project/builds",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers | middleware.AllowService,
			Middlewares: []gin.HandlerFunc{api.authorizeResource(policy.Rule{
				Identities:        policy.AllowUsers | policy.AllowService,
				ObjectAccess:      policy.ProjectMember,
				ServicePermission: iamv1.MachinePermissionBuildsCreate,
			})},
		},
		{
			Method:         http.MethodDelete,
			Path:           "/apis/ebs/v1/projects/:project/builds/:name",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers,
			Middlewares:    []gin.HandlerFunc{api.authorizeResource(policy.Rule{Identities: policy.AllowUsers, ObjectAccess: policy.ProjectOwner})},
		},
		{
			Method:         http.MethodPost,
			Path:           "/apis/ebs/v1/projects/:project/builds/:name/abort",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers | middleware.AllowService,
			Middlewares: []gin.HandlerFunc{api.authorizeResource(policy.Rule{
				Identities:        policy.AllowUsers | policy.AllowService,
				ObjectAccess:      policy.ProjectMember,
				ServicePermission: iamv1.MachinePermissionBuildsAbort,
			})},
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/jobs",
			Options:        middleware.RouteOptions{ListLimit: 100, Watch: true},
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
			Middlewares:    []gin.HandlerFunc{api.authorizeResource(policy.Rule{Identities: policy.AllowAnonymous | policy.AllowUsers, WatchAccess: policy.ProjectMember})},
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/jobs/:name",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers | middleware.AllowRunner,
			Middlewares:    []gin.HandlerFunc{api.runnerJobReadGuard},
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/jobs/:name/status",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodPatch,
			Path:           "/apis/ebs/v1/projects/:project/jobs/:name/status",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowRunner,
			Middlewares:    []gin.HandlerFunc{api.authorizeResource(policy.Rule{Identities: policy.AllowRunner}), api.prepareJobStatusUpdate},
		},
		{
			Method:         http.MethodPost,
			Path:           "/apis/ebs/v1/projects/:project/jobs/:name/abort",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers,
			Middlewares:    []gin.HandlerFunc{api.authorizeResource(policy.Rule{Identities: policy.AllowUsers, ObjectAccess: policy.ProjectMember})},
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/oldjobs",
			Options:        middleware.RouteOptions{ListLimit: 100},
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/oldjobs/:name",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/snapshots",
			Options:        middleware.RouteOptions{ListLimit: 100},
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/snapshots/:name",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/snapshots/:name/status",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/buildinfos",
			Options:        middleware.RouteOptions{ListLimit: 100},
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/buildinfos/:name",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/buildinfos/:name/status",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/buildgraphs",
			Options:        middleware.RouteOptions{ListLimit: 100},
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/buildgraphs/:name",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/buildgraphs/:name/status",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/rpmrepos",
			Options:        middleware.RouteOptions{ListLimit: 100},
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/rpmrepos/:name",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/projects/:project/rpmrepos/:name/status",
			Authentication: middleware.AuthenticationOptional,
			Identities:     middleware.AllowAnonymous | middleware.AllowUsers,
		},
	}
	for i := range routes {
		routes[i].Handler = api.upstream.Forward
	}
	registerRoutes(router, rules, routes)
}

func registerOtherRoutes(router *gin.Engine, rules map[middleware.RouteKey]middleware.RoutePolicy, api *Handler) {
	routes := []routeDefinition{
		{
			Method:         http.MethodGet,
			Path:           "/openapi.json",
			Authentication: middleware.AuthenticationNone,
			Handler:        openapi.Serve,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/build-targets",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowUsers,
			Handler:        api.buildTargets,
		},
		{
			Method:         http.MethodGet,
			Path:           "/apis/ebs/v1/service-endpoints",
			Authentication: middleware.AuthenticationRequired,
			Identities:     middleware.AllowRunner,
			Handler:        api.serviceEndpoints,
		},
	}
	registerRoutes(router, rules, routes)
}

func registerRoutes(router *gin.Engine, rules map[middleware.RouteKey]middleware.RoutePolicy, definitions []routeDefinition) {
	for _, route := range definitions {
		key := middleware.RouteKey{Method: route.Method, Path: route.Path}
		if _, exists := rules[key]; exists {
			panic(fmt.Sprintf("duplicate Gateway route: %s %s", route.Method, route.Path))
		}
		if route.Handler == nil {
			panic(fmt.Sprintf("Gateway route requires a handler: %s %s", route.Method, route.Path))
		}
		access := middleware.RoutePolicy{
			Authentication: route.Authentication,
			Identities:     route.Identities,
			Options:        route.Options,
		}
		if access.Authentication == middleware.AuthenticationNone && access.Identities == 0 {
			access.Identities = middleware.AllowAnonymous
		}
		if err := middleware.ValidateRoute(key, access); err != nil {
			panic(fmt.Sprintf("invalid Gateway route %s %s: %v", route.Method, route.Path, err))
		}
		for _, handler := range route.Middlewares {
			if handler == nil {
				panic(fmt.Sprintf("nil Gateway middleware: %s %s", route.Method, route.Path))
			}
		}
		rules[key] = access
		handlers := make([]gin.HandlerFunc, 0, len(route.Middlewares)+1)
		handlers = append(handlers, route.Middlewares...)
		handlers = append(handlers, route.Handler)
		router.Handle(route.Method, route.Path, handlers...)
	}
}
