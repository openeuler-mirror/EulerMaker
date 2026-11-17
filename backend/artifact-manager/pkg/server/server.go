package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"artifact-manager/pkg/auth"
	"artifact-manager/pkg/diagnostics"
	"artifact-manager/pkg/release"
	"artifact-manager/pkg/repository"
	"artifact-manager/pkg/signing"
	"artifact-manager/pkg/upload"
)

type Server struct {
	cfg           Config
	store         *upload.Store
	auth          auth.Authorizer
	repositories  *repository.Manager
	releases      *release.Manager
	router        *gin.Engine
	signer        *signing.Signer
	sealer        *upload.Sealer
	cleanupCancel context.CancelFunc
	cleanupDone   chan struct{}
}

func NewHandler(c Config, a auth.Authorizer) (*Server, error) {
	st, e := upload.NewStore(c.DataDir)
	if e != nil {
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
	s.startTemporaryCleanup(func(ctx context.Context) error {
		return st.CleanupTemporary(ctx, c.Upload.TemporaryUploadTTL)
	})
	return s, nil
}

func (s *Server) startTemporaryCleanup(cleanup func(context.Context) error) {
	ctx, cancel := context.WithCancel(context.Background())
	s.cleanupCancel, s.cleanupDone = cancel, make(chan struct{})
	go func() {
		defer close(s.cleanupDone)
		if err := cleanup(ctx); err != nil && !errors.Is(err, context.Canceled) {
			klog.ErrorS(nil, "upload cleanup-temporary failed",
				"component", "upload", "operation", "cleanup-temporary", "cause", diagnostics.Cause(err))
		}
	}()
}

func (s *Server) Close() {
	if s.cleanupCancel != nil {
		s.cleanupCancel()
		<-s.cleanupDone
	}
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
