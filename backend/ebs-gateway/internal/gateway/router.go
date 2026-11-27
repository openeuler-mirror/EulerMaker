package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/auth"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/oauth"
	"ebs-gateway/internal/openapi"
)

func NewRouter(api *Handler, authHandler *auth.Handler, oauthHandler *oauth.Handler) *gin.Engine {
	router := gin.New()
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.RemoveExtraSlash = false
	_ = router.SetTrustedProxies(nil)
	// Register probes before the business middleware chain to avoid auditing or throttling them.
	registerHealthz(router)
	access := middleware.NewRoutePolicies(routeRules())
	router.Use(
		middleware.Audit(api.now),
		gin.Recovery(),
		middleware.PrepareQuery(access),
		middleware.Authenticate(api.auth, access),
		middleware.LimitIdentity(api.limits),
		middleware.AdmitIdentity(access),
	)

	registerAuth(router, authHandler)
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

func registerAuth(router *gin.Engine, api *auth.Handler) {
	router.POST("/auth/register", api.RegisterUser)
	router.POST("/auth/login", api.Login)
	router.POST("/auth/check", api.CheckToken)
	router.PUT("/auth/users/:name/password", api.ChangePassword)
	router.POST("/auth/machine-token", api.MachineToken)

	router.POST("/auth/machineaccounts", api.RegisterMachine)
}

func registerIAM(router *gin.Engine, api *Handler) {
	iam := router.Group("/apis/iam.ebs/v1")

	iam.GET("/users", api.upstream.Forward)
	iam.GET("/users/:name", api.upstream.Forward)
	iam.PUT("/users/:name", api.prepareUserUpdate, api.upstream.Forward)
	iam.PATCH("/users/:name", api.prepareUserUpdate, api.upstream.Forward)
	iam.DELETE("/users/:name", api.protectAdminUser, api.upstream.Forward)

	iam.GET("/machineaccounts", api.upstream.Forward)
	iam.GET("/machineaccounts/:name", api.upstream.Forward)
	iam.PUT("/machineaccounts/:name", api.upstream.Forward)
	iam.PATCH("/machineaccounts/:name", api.upstream.Forward)
	iam.DELETE("/machineaccounts/:name", api.upstream.Forward)
}

func registerConfigProjections(router *gin.Engine, api *Handler) {
	projections := router.Group("/apis/ebs/v1")

	projections.GET("/build-targets", api.buildTargets)
	projections.GET("/service-endpoints", api.serviceEndpoints)
}

func registerConfigs(router *gin.Engine, api *Handler) {
	configs := router.Group("/apis/ebs/v1/configs")

	configs.GET("", api.upstream.Forward)
	configs.POST("", api.upstream.Forward)

	configs.GET("/:name", api.upstream.Forward)
	configs.PUT("/:name", api.upstream.Forward)
	configs.PATCH("/:name", api.upstream.Forward)
	configs.DELETE("/:name", rejectBuiltInConfigDeletion, api.upstream.Forward)
}

func registerScripts(router *gin.Engine, api *Handler) {
	scripts := router.Group("/apis/ebs/v1/scripts")
	scripts.GET("", api.upstream.Forward)
	scripts.POST("", api.upstream.Forward)
	scripts.GET("/:name", api.upstream.Forward)
	scripts.PUT("/:name", api.upstream.Forward)
	scripts.PATCH("/:name", api.upstream.Forward)
	scripts.DELETE("/:name", api.upstream.Forward)
}

func registerProjects(router *gin.Engine, api *Handler) {
	guard := api.projectGuard("")
	read := api.publicReadGuard("projects", "")
	router.GET("/apis/ebs/v1/projects", read, api.forwardResource)
	router.POST("/apis/ebs/v1/projects", guard, api.prepareProjectCreate, api.upstream.Forward)

	router.GET("/apis/ebs/v1/projects/:project", read, api.forwardResource)
	router.PUT("/apis/ebs/v1/projects/:project", guard, api.prepareProjectUpdate, api.upstream.Forward)
	router.PATCH("/apis/ebs/v1/projects/:project", guard, api.prepareProjectUpdate, api.upstream.Forward)
}

func registerRunners(router *gin.Engine, api *Handler) {
	runners := router.Group("/apis/ebs/v1/runners")

	runners.GET("", api.upstream.Forward)
	runners.POST("", api.prepareRunnerCreate, api.upstream.Forward)

	runners.GET("/:runner", api.runnerReadGuard, api.upstream.Forward)
	runners.PUT("/:runner", api.runnerUpdateGuard, api.upstream.Forward)
	runners.PATCH("/:runner", api.runnerUpdateGuard, api.upstream.Forward)
	runners.DELETE("/:runner", api.upstream.Forward)

	runners.GET("/:runner/status", api.upstream.Forward)
	runners.PUT("/:runner/status", api.runnerStatusUpdateGuard, api.upstream.Forward)
	runners.PATCH("/:runner/status", api.runnerStatusUpdateGuard, api.upstream.Forward)

	runners.GET("/:runner/jobs", api.prepareRunnerJobs, api.upstream.Forward)
}

func registerProjectScopedResources(router *gin.Engine, api *Handler) {
	project := router.Group("/apis/ebs/v1/projects/:project")
	guard := api.projectScopedGuard("builds", "")
	readBuild := api.publicReadGuard("builds", "")
	project.GET("/builds", readBuild, api.forwardResource)
	project.POST("/builds", guard, api.upstream.Forward)

	project.GET("/builds/:name", readBuild, api.forwardResource)
	project.DELETE("/builds/:name", guard, api.upstream.Forward)

	project.GET("/builds/:name/status", api.publicReadGuard("builds", "status"), api.forwardResource)

	guard = api.projectScopedGuard("builds", "abort")
	project.POST("/builds/:name/abort", guard, api.upstream.Forward)

	readJob := api.publicReadGuard("jobs", "")
	project.GET("/jobs", readJob, api.forwardResource)

	project.GET("/jobs/:name", readJob, api.runnerJobReadGuard, api.forwardResource)

	guard = api.projectScopedGuard("jobs", "status")
	project.GET("/jobs/:name/status", api.publicReadGuard("jobs", "status"), api.forwardResource)
	project.PATCH("/jobs/:name/status", guard, api.prepareJobStatusUpdate, api.upstream.Forward)

	guard = api.projectScopedGuard("jobs", "abort")
	project.POST("/jobs/:name/abort", guard, api.upstream.Forward)

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
