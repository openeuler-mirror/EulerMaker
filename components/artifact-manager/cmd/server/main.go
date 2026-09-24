package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	artifact "artifact-manager/pkg/artifact"
)

func main() {
	c, e := artifact.LoadConfig(os.Args[1:])
	if e != nil {
		log.Fatal(e)
	}
	a, e := artifact.NewGatewayAuthorizer(c)
	if e != nil {
		log.Fatal(e)
	}
	s, e := artifact.NewServer(c, a)
	if e != nil {
		log.Fatal(e)
	}
	log.Printf("artifact-manager listening on %s", c.Listen)
	serverError := make(chan error, 1)
	go func() { serverError <- s.ListenAndServe() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverError:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-signals:
		ctx, cancel := context.WithTimeout(context.Background(), c.ShutdownTimeout)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("artifact-manager shutdown failed: %v", err)
		}
		select {
		case <-serverError:
		case <-time.After(time.Second):
		}
	}
}
