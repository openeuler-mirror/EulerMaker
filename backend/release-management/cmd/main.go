package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"release-management/cmd/app"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	os.Exit(app.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
