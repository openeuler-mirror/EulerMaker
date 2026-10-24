package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"artifact-manager/pkg/auth"
	"artifact-manager/pkg/release"
	"artifact-manager/pkg/repository"
	"artifact-manager/pkg/signing"
	"artifact-manager/pkg/upload"
)

type Server struct {
	cfg          Config
	store        *upload.Store
	auth         auth.Authorizer
	repositories *repository.Manager
	releases     *release.Manager
	router       *gin.Engine
	signer       *signing.Signer
	sealer       *upload.Sealer
}

func NewHandler(c Config, a auth.Authorizer) (*Server, error) {
	st, e := upload.NewStore(c.DataDir)
	if e != nil {
		return nil, e
	}
	if e = st.CleanupTemporary(c.Upload.TemporaryUploadTTL); e != nil {
		return nil, e
	}
	signingConfig := c.Signing
	signingConfig.PublicKey = c.Release.PublicKey
	signer, err := signing.New(signingConfig)
	if err != nil {
		return nil, err
	}
	sealer, err := upload.NewSealer(c.Upload, signer, c.Repository.RPMQueryCommand, c.Signing.Workers, c.Signing.Timeout)
	if err != nil {
		_ = signer.Close()
		return nil, err
	}
	st.SetSealer(sealer)
	materializer := repository.NewFilesystemMaterializer(c.DataDir, c.Repository, st, signer.Fingerprint())
	s, err := newServer(c, a, st, materializer, signer.Fingerprint())
	if err != nil {
		sealer.Close()
		_ = signer.Close()
		return nil, err
	}
	s.signer = signer
	s.sealer = sealer
	return s, nil
}

func (s *Server) Close() {
	if s.sealer != nil {
		s.sealer.Close()
	}
	s.repositories.Stop()
	s.releases.Stop()
	if s.signer != nil {
		_ = s.signer.Close()
	}
}

func newServer(c Config, a auth.Authorizer, store *upload.Store, materializer repository.Materializer, signingFingerprint string) (*Server, error) {
	repositories, err := repository.NewManager(c.DataDir, c.Repository, materializer, signingFingerprint)
	if err != nil {
		return nil, err
	}
	releases, err := release.NewManager(c.DataDir, c.Release, repositories, release.NewFilesystemMaterializer(c.DataDir, c.Release, c.Repository))
	if err != nil {
		repositories.Stop()
		return nil, err
	}
	s := &Server{cfg: c, store: store, auth: a, repositories: repositories, releases: releases}
	s.router = s.newRouter()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}
