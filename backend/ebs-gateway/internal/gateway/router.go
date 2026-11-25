package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/oauth"
	"ebs-gateway/internal/openapi"
)

func NewRouter(api *Handler, oauthHandler *oauth.Handler) *gin.Engine {
	router := gin.New()
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.RemoveExtraSlash = false
	_ = router.SetTrustedProxies(nil)
	router.Use(middleware.Audit(api.now), gin.Recovery())

	registerAuth(router, api)
	registerOAuth(router, oauthHandler)
	registerIAM(router, api)

	registerServiceEndpoints(router, api)
	registerConfigs(router, api)
	registerScripts(router, api)
	registerProjects(router, api)
	registerRunners(router, api)
	registerProjectScopedResources(router, api)

	registerHealthz(router)
	registerOpenAPI(router)

	router.NoRoute(api.auth.Authenticate, func(c *gin.Context) {
		c.Status(http.StatusNotFound)
	})
	return router
}

func registerOAuth(router *gin.Engine, h *oauth.Handler) {
	router.GET("/auth/providers", h.Providers)
	router.GET("/auth/oauth/:provider/login", h.Login)
	router.POST("/auth/oauth/:provider/callback", h.Callback)
	router.POST("/auth/oauth/register", h.Register)
}

func registerAuth(router *gin.Engine, api *Handler) {
	router.POST("/auth/register", api.registerUser)
	router.POST("/auth/login", api.login)
	router.POST("/auth/check", api.auth.Authenticate, middleware.LimitAuthenticated(api.limits), api.checkToken)
	router.PUT("/auth/users/:name/password", api.auth.Authenticate, middleware.LimitAuthenticated(api.limits), api.changePassword)
	router.POST("/auth/machine-token", api.machineToken)
	router.POST("/auth/machineaccounts", api.auth.Authenticate, middleware.LimitAuthenticated(api.limits), middleware.RequireAdmin, api.registerMachine)
}

func registerIAM(router *gin.Engine, api *Handler) {
	iam := router.Group("/apis/iam.ebs/v1")
	iam.Use(api.auth.Authenticate, middleware.LimitAuthenticated(api.limits), middleware.RequireAdmin, rejectIAMWatch)

	iam.GET("/users", api.users)

	iam.GET("/users/:name", api.users)
	iam.PUT("/users/:name", api.users)
	iam.PATCH("/users/:name", api.users)
	iam.DELETE("/users/:name", api.users)

	iam.GET("/machineaccounts", api.machineAccounts)

	iam.GET("/machineaccounts/:name", api.machineAccounts)
	iam.PUT("/machineaccounts/:name", api.machineAccounts)
	iam.PATCH("/machineaccounts/:name", api.machineAccounts)
	iam.DELETE("/machineaccounts/:name", api.machineAccounts)
}

func registerServiceEndpoints(router *gin.Engine, api *Handler) {
	router.GET("/apis/ebs/v1/service-endpoints", api.auth.Authenticate,
		middleware.LimitAuthenticated(api.limits), api.serviceEndpoints)
}

func registerConfigs(router *gin.Engine, api *Handler) {
	guard := api.resourceGuard("configs", "", "name", "")
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPost}, "/apis/ebs/v1/configs", guard, api.forwardResource)
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete}, "/apis/ebs/v1/configs/:name", guard, api.forwardResource)
}

func registerScripts(router *gin.Engine, api *Handler) {
	guard := api.resourceGuard("scripts", "", "name", "")
	router.Match([]string{http.MethodGet, http.MethodPost}, "/apis/ebs/v1/scripts", guard, api.forwardResource)
	router.Match([]string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete}, "/apis/ebs/v1/scripts/:name", guard, api.forwardResource)
}

func registerProjects(router *gin.Engine, api *Handler) {
	guard := api.resourceGuard("projects", "", "project", "")
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPost}, "/apis/ebs/v1/projects", guard, api.forwardResource)
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch}, "/apis/ebs/v1/projects/:project", guard, api.forwardResource)

	guard = api.resourceGuard("projects", "", "project", "status")
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch}, "/apis/ebs/v1/projects/:project/status", guard, api.forwardResource)
}

func registerRunners(router *gin.Engine, api *Handler) {
	guard := api.resourceGuard("runners", "", "runner", "")
	router.GET("/apis/ebs/v1/runners", guard, api.forwardResource)
	router.POST("/apis/ebs/v1/runners", guard, api.forwardResource)

	router.GET("/apis/ebs/v1/runners/:runner", guard, api.forwardResource)
	router.PUT("/apis/ebs/v1/runners/:runner", guard, api.forwardResource)
	router.PATCH("/apis/ebs/v1/runners/:runner", guard, api.forwardResource)
	router.DELETE("/apis/ebs/v1/runners/:runner", guard, api.forwardResource)

	guard = api.resourceGuard("runners", "", "runner", "status")
	router.GET("/apis/ebs/v1/runners/:runner/status", guard, api.forwardResource)
	router.PUT("/apis/ebs/v1/runners/:runner/status", guard, api.forwardResource)
	router.PATCH("/apis/ebs/v1/runners/:runner/status", guard, api.forwardResource)

	guard = api.resourceGuard("runners", "", "runner", "jobs")
	router.GET("/apis/ebs/v1/runners/:runner/jobs", guard, api.runnerJobsListGuard, api.forwardResource)
}

func registerProjectScopedResources(router *gin.Engine, api *Handler) {
	project := router.Group("/apis/ebs/v1/projects/:project")
	guard := api.resourceGuard("builds", "project", "name", "")
	project.Match([]string{http.MethodGet, http.MethodHead, http.MethodPost}, "/builds", guard, api.forwardResource)
	project.Match([]string{http.MethodGet, http.MethodHead, http.MethodDelete}, "/builds/:name", guard, api.forwardResource)

	guard = api.resourceGuard("builds", "project", "name", "status")
	project.Match([]string{http.MethodGet, http.MethodHead}, "/builds/:name/status", guard, api.forwardResource)

	guard = api.resourceGuard("builds", "project", "name", "abort")
	project.POST("/builds/:name/abort", guard, api.forwardResource)

	guard = api.resourceGuard("jobs", "project", "name", "")
	project.Match([]string{http.MethodGet, http.MethodHead, http.MethodPost}, "/jobs", guard, api.forwardResource)
	project.Match([]string{http.MethodGet, http.MethodHead}, "/jobs/:name", guard, api.runnerJobReadGuard, api.forwardResource)
	project.Match([]string{http.MethodPut, http.MethodPatch, http.MethodDelete}, "/jobs/:name", guard, api.forwardResource)

	guard = api.resourceGuard("jobs", "project", "name", "status")
	project.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch}, "/jobs/:name/status", guard, api.forwardResource)

	guard = api.resourceGuard("jobs", "project", "name", "abort")
	project.POST("/jobs/:name/abort", guard, api.forwardResource)

	guard = api.resourceGuard("oldjobs", "project", "name", "")
	project.GET("/oldjobs", guard, api.forwardResource)
	project.GET("/oldjobs/:name", guard, api.forwardResource)

	for _, resource := range []string{"snapshots", "buildinfos", "buildgraphs", "rpmrepos"} {
		guard := api.resourceGuard(resource, "project", "name", "")
		project.GET("/"+resource, guard, api.forwardResource)
		project.GET("/"+resource+"/:name", guard, api.forwardResource)

		guard = api.resourceGuard(resource, "project", "name", "status")
		project.GET("/"+resource+"/:name/status", guard, api.forwardResource)
	}
}

func registerHealthz(router *gin.Engine) {
	router.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok\n") })
}

func registerOpenAPI(router *gin.Engine) {
	router.GET("/openapi.json", openapi.Serve)
}
