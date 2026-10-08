package artifact

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type Server struct {
	cfg          Config
	store        *Store
	auth         Authorizer
	repositories *repositoryManager
	releases     *releaseManager
	router       *gin.Engine
}

func NewHandler(c Config, a Authorizer) (*Server, error) {
	st, e := NewStore(c.DataDir)
	if e != nil {
		return nil, e
	}
	if e = st.CleanupTemporary(c.TemporaryUploadTTL); e != nil {
		return nil, e
	}
	return newArtifactServer(c, a, st, newFilesystemMaterializer(c, st))
}

func (s *Server) Close() {
	s.repositories.stop()
	s.releases.stop()
}

func newArtifactServer(c Config, a Authorizer, store *Store, materializer repositoryMaterializer) (*Server, error) {
	repositories, err := newRepositoryManager(c, materializer)
	if err != nil {
		return nil, err
	}
	releases, err := newReleaseManager(c, repositories, newFilesystemReleaseMaterializer(c))
	if err != nil {
		repositories.stop()
		return nil, err
	}
	s := &Server{cfg: c, store: store, auth: a, repositories: repositories, releases: releases}
	s.router = s.newRouter()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}
