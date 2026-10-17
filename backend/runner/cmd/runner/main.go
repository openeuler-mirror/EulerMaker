package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"ebs-runner/pkg/options"
	"ebs-runner/pkg/runner"
)

func main() {
	cfg, err := options.LoadConfig(os.Args[1:])
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
		_ = agent.Close()
		log.Fatalf("runner stopped: %v", err)
	}
	if err := agent.Close(); err != nil {
		log.Printf("close container runtime: %v", err)
	}
}
