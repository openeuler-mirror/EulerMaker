package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"git-server/pkg/auth"
	"git-server/pkg/command"
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
		RetryMax: config.RetryMaxDelay, CleanupPeriod: config.TempCleanupInterval,
	})
	if err != nil {
		log.Fatal(err)
	}
	commandExecutor := command.New(config.OperationTimeout, config.MaxCommandOutputBytes)
	clone, err := server.NewSmartHTTP(manager, config.OperationTimeout, config.CloneWorkers)
	if err != nil {
		log.Fatal(err)
	}
	handler := server.New(manager, commandExecutor, config.MaxRequestBodyBytes, clone)
	httpServer := &http.Server{
		Addr: config.ListenAddress, Handler: handler, ReadHeaderTimeout: config.OperationTimeout,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	managerDone := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(managerDone)
	}()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
		}
	}()
	log.Printf("git-server listening on %s", config.ListenAddress)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	stop()
	<-managerDone
	<-shutdownDone
}
