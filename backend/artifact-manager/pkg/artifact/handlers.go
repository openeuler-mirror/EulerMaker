package artifact

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"artifact-manager/pkg/signing"
)

type Server struct {
	cfg          Config
	store        *Store
	auth         Authorizer
	repositories *repositoryManager
	releases     *releaseManager
	router       *gin.Engine
	signer       *signing.Signer
}

func NewHandler(c Config, a Authorizer) (*Server, error) {
	st, e := NewStore(c.DataDir)
	if e != nil {
		return nil, e
	}
	if e = st.CleanupTemporary(c.TemporaryUploadTTL); e != nil {
		return nil, e
	}
	signer, err := signing.New(signing.Config{Mode: c.RPMSigningMode, PublicKey: c.ReleasePublicKey, GPGHome: c.RPMSigningGPGHome, SignatrustConfig: c.RPMSigningSignatrustConfig, SignatrustKeyName: c.RPMSigningSignatrustKeyName, Timeout: c.RPMSigningTimeout})
	if err != nil {
		return nil, err
	}
	c.signingFingerprint = signer.Fingerprint()
	s, err := newArtifactServer(c, a, st, newFilesystemMaterializer(c, st, signer))
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
