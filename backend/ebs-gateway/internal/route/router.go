package route

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/handler"
	"ebs-gateway/internal/openapi"
)

func New(api *handler.Handler) *gin.Engine {
	router := gin.New()
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.RemoveExtraSlash = false
	_ = router.SetTrustedProxies(nil)
	middleware := api.Middleware()
	router.Use(middleware.Audit, gin.Recovery())

	registerAuth(router, api, middleware)
	registerOAuth(router, api)
	registerIAM(router, api, middleware)

	registerServiceEndpoints(router, api, middleware)
	registerConfigs(router, api)
	registerScripts(router, api)
	registerProjects(router, api)
	registerRunners(router, api)
	registerProjectScopedResources(router, api)

	registerHealthz(router)
	registerOpenAPI(router)

	router.NoRoute(middleware.Authenticate, func(c *gin.Context) {
		c.Status(http.StatusNotFound)
	})
	return router
}

func registerOAuth(router *gin.Engine, api *handler.Handler) {
	router.GET("/auth/providers", api.OAuth.Providers)
	router.GET("/auth/oauth/:provider/login", api.OAuth.Login)
	router.POST("/auth/oauth/:provider/callback", api.OAuth.Callback)
	router.POST("/auth/oauth/register", api.OAuth.Register)
}

func registerAuth(router *gin.Engine, api *handler.Handler, middleware handler.Middleware) {
	router.POST("/auth/register", api.RegisterUser)
	router.POST("/auth/login", api.Login)
	router.POST("/auth/check", middleware.Authenticate, middleware.LimitAuthenticated, api.CheckToken)
	router.PUT("/auth/users/:name/password", middleware.Authenticate, middleware.LimitAuthenticated, api.ChangePassword)
	router.POST("/auth/machine-token", api.MachineToken)
	router.POST("/auth/machineaccounts", middleware.Authenticate, middleware.LimitAuthenticated, middleware.RequireAdmin, api.RegisterMachine)
}

func registerIAM(router *gin.Engine, api *handler.Handler, middleware handler.Middleware) {
	iam := router.Group("/apis/iam.ebs/v1")
	iam.Use(middleware.Authenticate, middleware.LimitAuthenticated, middleware.RequireAdmin, handler.RejectIAMWatch)

	iam.Match([]string{http.MethodGet, http.MethodHead}, "/users", api.Users)
	iam.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete}, "/users/:name", api.Users)
	iam.Match([]string{http.MethodGet, http.MethodHead}, "/machineaccounts", api.MachineAccounts)
	iam.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete}, "/machineaccounts/:name", api.MachineAccounts)
}

func registerServiceEndpoints(router *gin.Engine, api *handler.Handler, middleware handler.Middleware) {
	router.GET("/apis/ebs/v1/service-endpoints", middleware.Authenticate,
		middleware.LimitAuthenticated, api.ServiceEndpoints)
}

func registerConfigs(router *gin.Engine, api *handler.Handler) {
	guard := api.ResourceGuard("configs", "", "name", "")
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPost}, "/apis/ebs/v1/configs", guard, api.ForwardResource)
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete}, "/apis/ebs/v1/configs/:name", guard, api.ForwardResource)
}

func registerScripts(router *gin.Engine, api *handler.Handler) {
	guard := api.ResourceGuard("scripts", "", "name", "")
	router.Match([]string{http.MethodGet, http.MethodPost}, "/apis/ebs/v1/scripts", guard, api.ForwardResource)
	router.Match([]string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete}, "/apis/ebs/v1/scripts/:name", guard, api.ForwardResource)
}

func registerProjects(router *gin.Engine, api *handler.Handler) {
	guard := api.ResourceGuard("projects", "", "project", "")
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPost}, "/apis/ebs/v1/projects", guard, api.ForwardResource)
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch}, "/apis/ebs/v1/projects/:project", guard, api.ForwardResource)

	guard = api.ResourceGuard("projects", "", "project", "status")
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch}, "/apis/ebs/v1/projects/:project/status", guard, api.ForwardResource)
}

func registerRunners(router *gin.Engine, api *handler.Handler) {
	guard := api.ResourceGuard("runners", "", "runner", "")
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPost}, "/apis/ebs/v1/runners", guard, api.ForwardResource)
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete}, "/apis/ebs/v1/runners/:runner", guard, api.ForwardResource)

	guard = api.ResourceGuard("runners", "", "runner", "status")
	router.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch}, "/apis/ebs/v1/runners/:runner/status", guard, api.ForwardResource)

	guard = api.ResourceGuard("runners", "", "runner", "jobs")
	router.GET("/apis/ebs/v1/runners/:runner/jobs", guard, api.RunnerJobsListGuard, api.ForwardResource)
}

func registerProjectScopedResources(router *gin.Engine, api *handler.Handler) {
	project := router.Group("/apis/ebs/v1/projects/:project")
	guard := api.ResourceGuard("builds", "project", "name", "")
	project.Match([]string{http.MethodGet, http.MethodHead, http.MethodPost}, "/builds", guard, api.ForwardResource)
	project.Match([]string{http.MethodGet, http.MethodHead, http.MethodDelete}, "/builds/:name", guard, api.ForwardResource)

	guard = api.ResourceGuard("builds", "project", "name", "status")
	project.Match([]string{http.MethodGet, http.MethodHead}, "/builds/:name/status", guard, api.ForwardResource)

	guard = api.ResourceGuard("builds", "project", "name", "abort")
	project.POST("/builds/:name/abort", guard, api.ForwardResource)

	guard = api.ResourceGuard("jobs", "project", "name", "")
	project.Match([]string{http.MethodGet, http.MethodHead, http.MethodPost}, "/jobs", guard, api.ForwardResource)
	project.Match([]string{http.MethodGet, http.MethodHead}, "/jobs/:name", guard, api.RunnerJobReadGuard, api.ForwardResource)
	project.Match([]string{http.MethodPut, http.MethodPatch, http.MethodDelete}, "/jobs/:name", guard, api.ForwardResource)

	guard = api.ResourceGuard("jobs", "project", "name", "status")
	project.Match([]string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch}, "/jobs/:name/status", guard, api.ForwardResource)

	guard = api.ResourceGuard("jobs", "project", "name", "abort")
	project.POST("/jobs/:name/abort", guard, api.ForwardResource)

	guard = api.ResourceGuard("oldjobs", "project", "name", "")
	project.GET("/oldjobs", guard, api.ForwardResource)
	project.GET("/oldjobs/:name", guard, api.ForwardResource)

	for _, resource := range []string{"snapshots", "buildinfos", "buildgraphs", "rpmrepos"} {
		guard := api.ResourceGuard(resource, "project", "name", "")
		project.GET("/"+resource, guard, api.ForwardResource)
		project.GET("/"+resource+"/:name", guard, api.ForwardResource)

		guard = api.ResourceGuard(resource, "project", "name", "status")
		project.GET("/"+resource+"/:name/status", guard, api.ForwardResource)
	}
}

func registerHealthz(router *gin.Engine) {
	router.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok\n") })
}

func registerOpenAPI(router *gin.Engine) {
	router.GET("/openapi.json", openapi.Serve)
}
