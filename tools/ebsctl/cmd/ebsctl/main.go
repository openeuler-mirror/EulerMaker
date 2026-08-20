package main

import (
	"context"
	"os"
	"syscall"

	"ebsctl/pkg/cli"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	signalNotify(signals, os.Interrupt, syscall.SIGTERM)
	defer signalStop(signals)
	go func() {
		<-signals
		cancel()
		<-signals
		os.Exit(130)
	}()
	code := cli.Execute(ctx, cli.Streams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}, os.Args[1:])
	os.Exit(code)
}
