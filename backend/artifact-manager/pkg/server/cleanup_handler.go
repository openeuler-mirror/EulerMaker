package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Server) deleteBuildRepositories(c *gin.Context) {
	w, r := c.Writer, c.Request
	if r.Method != http.MethodDelete {
		method(w, http.MethodDelete)
		return
	}
	project, arch, build := c.Param("project"), c.Param("arch"), c.Param("build")
	if !validIdentifier(project) || !validIdentifier(arch) || !validIdentifier(build) || r.ContentLength != 0 {
		writeErr(w, r, http.StatusBadRequest, "InvalidRequest", "invalid cleanup scope or nonempty body", false, nil)
		return
	}
	// Check current and drain release readers before deleting their process repository source.
	done, err := s.releases.DeleteBuild(project, arch, build)
	if err != nil {
		s.writeReleaseError(w, r, err)
		return
	}
	if done {
		done, err = s.repositories.DeleteBuild(project, arch, build)
		if err != nil {
			s.writeRepositoryError(w, r, err)
			return
		}
	}
	if !done {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
