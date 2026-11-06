package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"artifact-manager/pkg/repository"
)

// prepareBuildCleanup durably fences both producers and waits for accepted work before deleting any files.
func (s *Server) prepareBuildCleanup(c *gin.Context) bool {
	project, build := c.Param("project"), c.Param("build")
	if !validIdentifier(project) || !validIdentifier(build) || c.Request.ContentLength != 0 {
		writeErr(c.Writer, c.Request, http.StatusBadRequest, "InvalidRequest", "invalid cleanup scope or nonempty body", false, nil)
		return false
	}
	done, err := s.releases.StopBuild(project, build)
	if err != nil {
		var repoErr *repository.Error
		if errors.As(err, &repoErr) {
			s.writeRepositoryError(c.Writer, c.Request, err)
		} else {
			s.writeReleaseError(c.Writer, c.Request, err)
		}
		return false
	}
	if !done {
		c.Header("Retry-After", "5")
		c.Status(http.StatusAccepted)
		return false
	}
	return true
}

func (s *Server) deleteBuildRepositories(c *gin.Context) {
	if !s.prepareBuildCleanup(c) {
		return
	}
	done, err := s.repositories.DeleteBuild(c.Param("project"), c.Param("build"))
	if err != nil {
		s.writeRepositoryError(c.Writer, c.Request, err)
		return
	}
	writeCleanupResult(c, done)
}

func (s *Server) deleteBuildReleases(c *gin.Context) {
	if !s.prepareBuildCleanup(c) {
		return
	}
	done, err := s.releases.DeleteBuild(c.Param("project"), c.Param("build"))
	if err != nil {
		s.writeReleaseError(c.Writer, c.Request, err)
		return
	}
	writeCleanupResult(c, done)
}

func writeCleanupResult(c *gin.Context, done bool) {
	if !done {
		c.Header("Retry-After", "5")
		c.Status(http.StatusAccepted)
		return
	}
	c.Status(http.StatusNoContent)
}
