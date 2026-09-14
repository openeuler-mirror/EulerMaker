package artifact

import (
	"net/http"
	"strings"
	"time"
)

type Server struct {
	cfg          Config
	store        *Store
	auth         Authorizer
	repositories *repositoryManager
	releases     *releaseManager
}

func NewServer(c Config, a Authorizer) (*http.Server, error) {
	st, e := NewStore(c.DataDir)
	if e != nil {
		return nil, e
	}
	if e = st.CleanupTemporary(c.TemporaryUploadTTL); e != nil {
		return nil, e
	}
	s, e := newArtifactServer(c, a, st, newFilesystemMaterializer(c, st))
	if e != nil {
		return nil, e
	}
	server := &http.Server{Addr: c.Listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	server.RegisterOnShutdown(func() { s.repositories.stop(); s.releases.stop() })
	return server, nil
}
func NewHandler(c Config, a Authorizer) (http.Handler, error) {
	st, e := NewStore(c.DataDir)
	if e != nil {
		return nil, e
	}
	if e = st.CleanupTemporary(c.TemporaryUploadTTL); e != nil {
		return nil, e
	}
	return newArtifactServer(c, a, st, newFilesystemMaterializer(c, st))
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
	return &Server{cfg: c, store: store, auth: a, repositories: repositories, releases: releases}, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		w.WriteHeader(200)
		w.Write([]byte("ok\n"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/internal/v1/repositories/") || r.URL.Path == "/internal/v1/repositories" {
		s.routeRepositoryManagement(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/internal/v1/releases/") || r.URL.Path == "/internal/v1/releases" {
		s.routeReleaseManagement(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/repositories/releases/v1/") {
		s.releaseVersionContent(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/repositories/v1/") {
		s.repositoryContent(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/repositories/") {
		s.releaseCurrentContent(w, r)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "artifacts" && parts[1] == "v1" {
		s.route(w, r, parts[2:])
		return
	}
	writeErr(w, r, 404, "NotFound", "not found", false, nil)
}
