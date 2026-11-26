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
		middleware.AdmitIdentity,
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

	router.POST("/auth/machineaccounts", api.registerMachine)
}

func registerIAM(router *gin.Engine, api *Handler) {
	iam := router.Group("/apis/iam.ebs/v1")

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
	projections := router.Group("/apis/ebs/v1")

	projections.GET("/build-targets", api.buildTargets)
	projections.GET("/service-endpoints", api.serviceEndpoints)
}

func registerConfigs(router *gin.Engine, api *Handler) {
	configs := router.Group("/apis/ebs/v1/configs")

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
	scripts.POST("", api.forward)
	scripts.GET("/:name", api.forward)
	scripts.PUT("/:name", api.forward)
	scripts.PATCH("/:name", api.forward)
	scripts.DELETE("/:name", api.forward)
}

func registerProjects(router *gin.Engine, api *Handler) {
	guard := api.projectGuard("")
	read := api.publicReadGuard("projects", "")
	router.GET("/apis/ebs/v1/projects", read, api.forwardResource)
	router.POST("/apis/ebs/v1/projects", guard, api.forwardResource)

	router.GET("/apis/ebs/v1/projects/:project", read, api.forwardResource)
	router.PUT("/apis/ebs/v1/projects/:project", guard, api.forwardResource)
	router.PATCH("/apis/ebs/v1/projects/:project", guard, api.forwardResource)

	statusGuard := api.projectGuard("status")
	router.GET("/apis/ebs/v1/projects/:project/status", api.publicReadGuard("projects", "status"), api.forwardResource)
	router.PUT("/apis/ebs/v1/projects/:project/status", statusGuard, api.forwardResource)
	router.PATCH("/apis/ebs/v1/projects/:project/status", statusGuard, api.forwardResource)
}

func registerRunners(router *gin.Engine, api *Handler) {
	runners := router.Group("/apis/ebs/v1/runners")

	runners.GET("", api.forward)
	runners.POST("", api.prepareRunnerCreate, api.forward)

	runners.GET("/:runner", api.runnerReadGuard, api.forward)
	runners.PUT("/:runner", api.runnerUpdateGuard, api.forward)
	runners.PATCH("/:runner", api.runnerUpdateGuard, api.forward)
	runners.DELETE("/:runner", api.runnerDeleteGuard, api.forward)

	runners.GET("/:runner/status", api.forward)
	runners.PUT("/:runner/status", api.runnerStatusUpdateGuard, api.forward)
	runners.PATCH("/:runner/status", api.runnerStatusUpdateGuard, api.forward)

	runners.GET("/:runner/jobs", api.prepareRunnerJobs, api.forward)
}

func registerProjectScopedResources(router *gin.Engine, api *Handler) {
	project := router.Group("/apis/ebs/v1/projects/:project")
	guard := api.projectScopedGuard("builds", "")
	readBuild := api.publicReadGuard("builds", "")
	project.GET("/builds", readBuild, api.forwardResource)
	project.POST("/builds", guard, api.forwardResource)

	project.GET("/builds/:name", readBuild, api.forwardResource)
	project.DELETE("/builds/:name", guard, api.forwardResource)

	project.GET("/builds/:name/status", api.publicReadGuard("builds", "status"), api.forwardResource)

	guard = api.projectScopedGuard("builds", "abort")
	project.POST("/builds/:name/abort", guard, api.forwardResource)

	guard = api.projectScopedGuard("jobs", "")
	readJob := api.publicReadGuard("jobs", "")
	project.GET("/jobs", readJob, api.forwardResource)
	project.POST("/jobs", guard, api.forwardResource)

	project.GET("/jobs/:name", readJob, api.runnerJobReadGuard, api.forwardResource)
	project.PUT("/jobs/:name", guard, api.forwardResource)
	project.PATCH("/jobs/:name", guard, api.forwardResource)
	project.DELETE("/jobs/:name", guard, api.forwardResource)

	guard = api.projectScopedGuard("jobs", "status")
	project.GET("/jobs/:name/status", api.publicReadGuard("jobs", "status"), api.forwardResource)
	project.PUT("/jobs/:name/status", guard, api.forwardResource)
	project.PATCH("/jobs/:name/status", guard, api.forwardResource)

	guard = api.projectScopedGuard("jobs", "abort")
	project.POST("/jobs/:name/abort", guard, api.forwardResource)

	readOldJob := api.publicReadGuard("oldjobs", "")
	project.GET("/oldjobs", readOldJob, api.forwardResource)
	project.GET("/oldjobs/:name", readOldJob, api.forwardResource)

	for _, resource := range []string{"snapshots", "buildinfos", "buildgraphs", "rpmrepos"} {
		read := api.publicReadGuard(resource, "")
		project.GET("/"+resource, read, api.forwardResource)
		project.GET("/"+resource+"/:name", read, api.forwardResource)
		project.GET("/"+resource+"/:name/status", api.publicReadGuard(resource, "status"), api.forwardResource)
	}
}

func registerHealthz(router *gin.Engine) {
	router.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok\n") })
}

func registerOpenAPI(router *gin.Engine) {
	router.GET("/openapi.json", openapi.Serve)
}
