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
	// Register probes before the business middleware chain to avoid auditing or throttling them.
	registerHealthz(router)
	router.Use(
		middleware.Audit(api.now),
		gin.Recovery(),
		middleware.ValidateWatchRoute,
		middleware.LimitIP(api.ipLimits),
		api.auth.AuthenticateRequest,
		middleware.LimitIdentity(api.limits),
	)

	registerAuth(router, api)
	registerOAuth(router, oauthHandler)
	registerIAM(router, api)

	registerConfigProjections(router, api)
	registerConfigs(router, api)
	registerScripts(router, api)
	registerProjects(router, api)
	registerRunners(router, api)
	registerProjectScopedResources(router, api)

	registerOpenAPI(router)

	router.NoRoute(func(c *gin.Context) {
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
	router.POST("/auth/check", api.checkToken)
	router.PUT("/auth/users/:name/password", api.changePassword)
	router.POST("/auth/machine-token", api.machineToken)
	router.POST("/auth/machineaccounts", middleware.RequireAdmin, api.registerMachine)
}

func registerIAM(router *gin.Engine, api *Handler) {
	iam := router.Group("/apis/iam.ebs/v1")
	iam.Use(middleware.RequireAdmin)

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

func registerConfigProjections(router *gin.Engine, api *Handler) {
	router.GET("/apis/ebs/v1/build-targets", api.buildTargets)
	router.GET("/apis/ebs/v1/service-endpoints", api.serviceEndpoints)
}

func registerConfigs(router *gin.Engine, api *Handler) {
	configs := router.Group("/apis/ebs/v1/configs")
	configs.Use(middleware.RequirePrivileged)

	configs.GET("", api.forward)
	configs.POST("", api.forward)

	configs.GET("/:name", api.forward)
	configs.PUT("/:name", api.forward)
	configs.PATCH("/:name", api.forward)
	configs.DELETE("/:name", rejectBuiltInConfigDeletion, api.forward)
}

func registerScripts(router *gin.Engine, api *Handler) {
	scripts := router.Group("/apis/ebs/v1/scripts")
	scripts.GET("", api.forward)
	scripts.GET("/:name", api.forward)

	scripts.Use(middleware.RequirePrivileged)

	scripts.POST("", api.forward)
	scripts.PUT("/:name", api.forward)
	scripts.PATCH("/:name", api.forward)
	scripts.DELETE("/:name", api.forward)
}

func registerProjects(router *gin.Engine, api *Handler) {
	guard := api.resourceGuard("projects", "", "project", "")
	router.GET("/apis/ebs/v1/projects", guard, api.forwardResource)
	router.POST("/apis/ebs/v1/projects", guard, api.forwardResource)

	router.GET("/apis/ebs/v1/projects/:project", guard, api.forwardResource)
	router.PUT("/apis/ebs/v1/projects/:project", guard, api.forwardResource)
	router.PATCH("/apis/ebs/v1/projects/:project", guard, api.forwardResource)

	guard = api.resourceGuard("projects", "", "project", "status")
	router.GET("/apis/ebs/v1/projects/:project/status", guard, api.forwardResource)
	router.PUT("/apis/ebs/v1/projects/:project/status", guard, api.forwardResource)
	router.PATCH("/apis/ebs/v1/projects/:project/status", guard, api.forwardResource)
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
	project.GET("/builds", guard, api.forwardResource)
	project.POST("/builds", guard, api.forwardResource)

	project.GET("/builds/:name", guard, api.forwardResource)
	project.DELETE("/builds/:name", guard, api.forwardResource)

	guard = api.resourceGuard("builds", "project", "name", "status")
	project.GET("/builds/:name/status", guard, api.forwardResource)

	guard = api.resourceGuard("builds", "project", "name", "abort")
	project.POST("/builds/:name/abort", guard, api.forwardResource)

	guard = api.resourceGuard("jobs", "project", "name", "")
	project.GET("/jobs", guard, api.forwardResource)
	project.POST("/jobs", guard, api.forwardResource)

	project.GET("/jobs/:name", guard, api.runnerJobReadGuard, api.forwardResource)
	project.PUT("/jobs/:name", guard, api.forwardResource)
	project.PATCH("/jobs/:name", guard, api.forwardResource)
	project.DELETE("/jobs/:name", guard, api.forwardResource)

	guard = api.resourceGuard("jobs", "project", "name", "status")
	project.GET("/jobs/:name/status", guard, api.forwardResource)
	project.PUT("/jobs/:name/status", guard, api.forwardResource)
	project.PATCH("/jobs/:name/status", guard, api.forwardResource)

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
