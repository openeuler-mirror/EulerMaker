package artifact

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"artifact-manager/pkg/auth"
	"artifact-manager/pkg/signing"
	"artifact-manager/pkg/upload"
)

type Server struct {
	cfg          Config
	store        *upload.Store
	auth         auth.Authorizer
	repositories *repositoryManager
	releases     *releaseManager
	router       *gin.Engine
	signer       *signing.Signer
}

func NewHandler(c Config, a auth.Authorizer) (*Server, error) {
	st, e := upload.NewStore(c.DataDir)
	if e != nil {
		return nil, e
	}
	if e = st.CleanupTemporary(c.Upload.TemporaryUploadTTL); e != nil {
		return nil, e
	}
	signer, err := signing.New(signing.Config{Mode: c.Signing.Mode, PublicKey: c.Release.PublicKey, GPGHome: c.Signing.GPGHome, SignatrustConfig: c.Signing.SignatrustConfig, SignatrustKeyName: c.Signing.SignatrustKeyName, Timeout: c.Signing.Timeout})
	if err != nil {
		return nil, err
	}
	s, err := newArtifactServer(c, a, st, newFilesystemMaterializer(c, st, signer), signer.Fingerprint())
	if err != nil {
		_ = signer.Close()
		return nil, err
	}
	s.signer = signer
	return s, nil
}

func (s *Server) Close() {
	s.repositories.stop()
	s.releases.stop()
	if s.signer != nil {
		_ = s.signer.Close()
	}
}

func newArtifactServer(c Config, a auth.Authorizer, store *upload.Store, materializer repositoryMaterializer, signingFingerprint string) (*Server, error) {
	repositories, err := newRepositoryManager(c, materializer, signingFingerprint)
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
