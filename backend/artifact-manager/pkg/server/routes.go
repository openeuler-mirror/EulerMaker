package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) newRouter() *gin.Engine {
	router := gin.New()
	router.Use(failureLogger())
	router.RedirectTrailingSlash = false
	router.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Next()
	})
	router.NoRoute(func(c *gin.Context) {
		writeErr(c.Writer, c.Request, http.StatusNotFound, "NotFound", "not found", false, nil)
	})

	health := func(c *gin.Context) {
		c.String(http.StatusOK, "ok\n")
	}
	router.Any("/healthz", health)
	router.Any("/readyz", health)

	internal := router.Group("/internal/v1")
	builds := internal.Group("/projects/:project/builds/:build")
	builds.Use(func(c *gin.Context) {
		if !validIdentifier(c.Param("project")) || !validIdentifier(c.Param("build")) {
			writeErr(c.Writer, c.Request, http.StatusBadRequest, "InvalidRequest", "invalid project or build name", false, nil)
			c.Abort()
		}
	})
	builds.POST("/repositories", s.createRepository)
	builds.GET("/repositories/:uid", s.getRepository)
	builds.DELETE("/repositories", s.deleteBuildRepositories)
	builds.POST("/releases", s.createRelease)
	builds.GET("/releases", s.getRelease)
	builds.DELETE("/releases", s.deleteBuildReleases)
	internal.Any("/projects/:project/jobs/:job", s.jobMethod(http.MethodDelete, s.deleteJobData))

	router.Any("/repositories/*path", s.routeRepositoryContent)

	jobs := router.Group("/artifacts/v1/projects/:project/jobs/:job")
	jobs.Use(func(c *gin.Context) {
		if !validIdentifier(c.Param("project")) || !validIdentifier(c.Param("job")) {
			writeErr(c.Writer, c.Request, http.StatusBadRequest, "InvalidRequest", "invalid project or job name", false, nil)
			c.Abort()
		}
	})
	jobs.Any("/artifacts", func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost:
			s.upload(c.Writer, c.Request, c.Param("project"), c.Param("job"))
		case http.MethodGet:
			s.list(c.Writer, c.Request, c.Param("project"), c.Param("job"))
		default:
			method(c.Writer, "GET, POST")
		}
	})
	jobs.GET("/artifacts/:id/content", func(c *gin.Context) {
		s.download(c.Writer, c.Request, c.Param("project"), c.Param("job"), c.Param("id"))
	})
	jobs.Any("/manifest/complete", s.jobMethod(http.MethodPost, s.completeManifest))
	jobs.Any("/manifest", s.jobMethod(http.MethodGet, s.getManifest))
	jobs.Any("/logs/chunks", s.jobMethod(http.MethodPost, s.appendLog))
	jobs.Any("/logs/status", s.jobMethod(http.MethodGet, s.logStatus))
	jobs.Any("/logs/content", s.jobMethod(http.MethodGet, s.logContent))
	jobs.Any("/logs/stream", s.jobMethod(http.MethodGet, s.logSSE))
	jobs.Any("/logs/complete", s.jobMethod(http.MethodPost, s.completeLog))
	return router
}

func (s *Server) jobMethod(method string, handler func(http.ResponseWriter, *http.Request, string, string)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != method {
			notFound(c)
			return
		}
		handler(c.Writer, c.Request, c.Param("project"), c.Param("job"))
	}
}

func (s *Server) routeRepositoryContent(c *gin.Context) {
	parts := strings.SplitN(strings.TrimPrefix(c.Request.URL.Path, "/repositories/"), "/", 5)
	if len(parts) == 5 && parts[1] == "builds" && validHash(parts[3]) {
		s.repositoryContent(c.Writer, c.Request)
		return
	}
	s.releaseCurrentContent(c.Writer, c.Request)
}

func notFound(c *gin.Context) {
	writeErr(c.Writer, c.Request, http.StatusNotFound, "NotFound", "not found", false, nil)
}
