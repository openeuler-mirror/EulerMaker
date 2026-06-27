package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"ebs-runner/pkg/runner"
)

func main() {
	cfg, err := runner.LoadConfig(os.Args[1:])
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agent, err := runner.NewAgent(cfg)
	if err != nil {
		log.Fatalf("create runner: %v", err)
	}
	if err := agent.Run(ctx); err != nil {
		log.Fatalf("runner stopped: %v", err)
	}
}
