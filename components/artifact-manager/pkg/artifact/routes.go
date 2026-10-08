package artifact

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) newRouter() *gin.Engine {
	router := gin.New()
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
	internal.Any("/repositories", func(c *gin.Context) {
		s.routeRepositoryManagement(c.Writer, c.Request)
	})
	internal.Any("/repositories/*path", func(c *gin.Context) {
		s.routeRepositoryManagement(c.Writer, c.Request)
	})
	internal.Any("/releases", func(c *gin.Context) {
		s.routeReleaseManagement(c.Writer, c.Request)
	})
	internal.Any("/releases/*path", func(c *gin.Context) {
		s.routeReleaseManagement(c.Writer, c.Request)
	})

	router.Any("/repositories/*path", s.routeRepositoryContent)
	router.Any("/artifacts/v1/artifacts/:id/content", func(c *gin.Context) {
		if c.Request.Method != http.MethodGet {
			notFound(c)
			return
		}
		s.download(c.Writer, c.Request, c.Param("id"))
	})

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
	path := c.Request.URL.Path
	if strings.HasPrefix(path, "/repositories/releases/v1/") {
		name := strings.SplitN(strings.TrimPrefix(path, "/repositories/releases/v1/"), "/", 2)[0]
		if _, ok := s.releases.get(name); ok {
			s.releaseVersionContent(c.Writer, c.Request)
			return
		}
	}
	if strings.HasPrefix(path, "/repositories/v1/") && validHash(strings.SplitN(strings.TrimPrefix(path, "/repositories/v1/"), "/", 2)[0]) {
		s.repositoryContent(c.Writer, c.Request)
		return
	}
	s.releaseCurrentContent(c.Writer, c.Request)
}

func notFound(c *gin.Context) {
	writeErr(c.Writer, c.Request, http.StatusNotFound, "NotFound", "not found", false, nil)
}
