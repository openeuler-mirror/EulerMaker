package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"git-server/pkg/auth"
	"git-server/pkg/command"
	"git-server/pkg/daemon"
	"git-server/pkg/gitops"
	"git-server/pkg/options"
	"git-server/pkg/repository"
	"git-server/pkg/server"
	"git-server/pkg/storage"
)

func main() {
	if handled, code := auth.RunHelper(os.Args[1:]); handled {
		os.Exit(code)
	}
	config, err := options.Parse(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	store, err := storage.Open(config.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	authManager, err := auth.Load(config.AuthConfig, store, config.AllowInsecureHTTPAuth)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	gitClient := gitops.New(authManager, config.OperationTimeout)
	manager, err := repository.NewManager(ctx, store, gitClient, repository.ManagerConfig{
		Workers: config.Workers, MaxRetries: config.MaxRetries, RetryBase: config.RetryBaseDelay,
		RetryMax: config.RetryMaxDelay, CloneBaseURL: config.CloneBaseURL, CleanupPeriod: config.TempCleanupInterval,
	})
	if err != nil {
		log.Fatal(err)
	}
	commandExecutor := command.New(config.OperationTimeout, config.MaxCommandOutputBytes)
	handler := server.New(manager, commandExecutor, config.MaxRequestBodyBytes)
	httpServer := &http.Server{Addr: config.ListenAddress, Handler: handler, ReadHeaderTimeout: config.OperationTimeout}

	managerDone := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(managerDone)
	}()
	if config.GitDaemonEnabled {
		go func() {
			err := daemon.Run(ctx, config.GitDaemonAddress, store.DataDir())
			if ctx.Err() == nil {
				log.Printf("git daemon stopped unexpectedly: %v", err)
				stop()
			}
		}()
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	log.Printf("git-server listening on %s", config.ListenAddress)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	stop()
	<-managerDone
}
