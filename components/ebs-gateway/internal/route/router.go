package route

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/handler"
)

func New(api *handler.Handler) *gin.Engine {
	router := gin.New()
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.RemoveExtraSlash = false
	_ = router.SetTrustedProxies(nil)
	middleware := api.Middleware()
	endpoints := api.Endpoints()
	router.Use(middleware.Audit, gin.Recovery())
	router.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok\n") })
	registerAuth(router, middleware, endpoints)
	registerIAM(router, api, middleware)
	registerResources(router, api)
	router.NoRoute(middleware.Authenticate, middleware.ResolveUser, func(c *gin.Context) {
		c.Status(http.StatusNotFound)
	})
	return router
}

func registerAuth(router *gin.Engine, middleware handler.Middleware, endpoints handler.Endpoints) {
	auth := router.Group("/auth")
	auth.POST("/register", endpoints.RegisterUser)
	auth.POST("/login", endpoints.Login)
	auth.POST("/runner-token", endpoints.RunnerToken)
	auth.POST("/check", middleware.Authenticate, middleware.LimitAuthenticated, endpoints.CheckToken)
	auth.PUT("/users/:name/password", middleware.Authenticate, middleware.ResolveUser, middleware.LimitAuthenticated, endpoints.ChangePassword)
	auth.POST("/machineaccounts", middleware.Authenticate, middleware.ResolveUser, middleware.LimitAuthenticated, middleware.RequireAdmin, endpoints.RegisterMachine)
}

func registerIAM(router *gin.Engine, api *handler.Handler, middleware handler.Middleware) {
	iam := router.Group("/apis/iam.ebs/v1")
	iam.Use(middleware.Authenticate, middleware.ResolveUser, middleware.LimitAuthenticated, middleware.RequireAdmin)
	users := iam.Group("/users")
	users.Any("", api.IAM("users"))
	users.Any("/:name", api.IAM("users"))
	machines := iam.Group("/machineaccounts")
	machines.Any("", api.IAM("machineaccounts"))
	machines.Any("/:name", api.IAM("machineaccounts"))
}

func registerResources(router *gin.Engine, api *handler.Handler) {
	group := router.Group("/apis/ebs/v1")
	registerCollection(group, api, "projects", "project")
	registerCollection(group, api, "runners", "runner")
	registerCollection(group, api, "configs", "name")
	registerCollection(group, api, "scripts", "name")
	for _, resource := range []string{"snapshots", "buildinfos", "rpmrepos"} {
		project := group.Group("/projects/:project/" + resource)
		registerReadOnlyResource(project, api, "", resource, "")
		registerReadOnlyResource(project, api, "/:name", resource, "")
		registerReadOnlyResource(project, api, "/:name/status", resource, "status")
	}
	for _, resource := range []string{"builds", "jobs"} {
		project := group.Group("/projects/:project/" + resource)
		registerResource(project, api, "", resource, "project", "name", "")
		registerResource(project, api, "/:name", resource, "project", "name", "")
		registerResource(project, api, "/:name/status", resource, "project", "name", "status")
		registerResource(project, api, "/:name/abort", resource, "project", "name", "abort")
	}
}

func registerReadOnlyResource(group *gin.RouterGroup, api *handler.Handler, path, resource, subresource string) {
	guard, endpoint := api.ResourceHandlers(resource, "project", "name", subresource)
	group.GET(path, guard, endpoint)
}

func registerCollection(parent *gin.RouterGroup, api *handler.Handler, resource, nameKey string) {
	group := parent.Group("/" + resource)
	registerResource(group, api, "", resource, "", nameKey, "")
	registerResource(group, api, "/:"+nameKey, resource, "", nameKey, "")
	if resource == "projects" || resource == "runners" {
		registerResource(group, api, "/:"+nameKey+"/status", resource, "", nameKey, "status")
	}
	if resource == "runners" {
		registerResource(group, api, "/:"+nameKey+"/jobs", resource, "", nameKey, "jobs")
	}
}

func registerResource(group *gin.RouterGroup, api *handler.Handler, path, resource, projectKey, nameKey, subresource string) {
	guard, endpoint := api.ResourceHandlers(resource, projectKey, nameKey, subresource)
	group.Any(path, guard, endpoint)
}
