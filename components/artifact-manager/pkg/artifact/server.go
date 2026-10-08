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
	path := r.URL.Path
	switch {
	case path == "/healthz" || path == "/readyz":
		w.WriteHeader(200)
		w.Write([]byte("ok\n"))
	case path == "/internal/v1/repositories" || strings.HasPrefix(path, "/internal/v1/repositories/"):
		s.routeRepositoryManagement(w, r)
	case path == "/internal/v1/releases" || strings.HasPrefix(path, "/internal/v1/releases/"):
		s.routeReleaseManagement(w, r)
	case strings.HasPrefix(path, "/repositories/"):
		s.routeRepositoryContent(w, r)
	case strings.HasPrefix(path, "/artifacts/v1/"):
		s.routeArtifactAPI(w, r, strings.Split(strings.TrimRight(path[len("/artifacts/v1/"):], "/"), "/"))
	default:
		writeErr(w, r, 404, "NotFound", "not found", false, nil)
	}
}

func (s *Server) routeRepositoryContent(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/repositories/releases/v1/") {
		name, _, _ := strings.Cut(strings.TrimPrefix(path, "/repositories/releases/v1/"), "/")
		if _, ok := s.releases.get(name); ok {
			s.releaseVersionContent(w, r)
			return
		}
	}
	if strings.HasPrefix(path, "/repositories/v1/") {
		uid, _, _ := strings.Cut(strings.TrimPrefix(path, "/repositories/v1/"), "/")
		if validHash(uid) {
			s.repositoryContent(w, r)
			return
		}
	}
	s.releaseCurrentContent(w, r)
}
