package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"scheduler/pkg/client"
	"scheduler/pkg/options"
	"scheduler/pkg/scheduler"
)

func main() {
	o, err := options.Parse(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	if o.InsecureSkipVerify {
		log.Print("WARNING: TLS server certificate verification is disabled")
	}
	config, err := o.RESTConfig()
	if err != nil {
		log.Fatal(err)
	}
	apiClient, err := client.New(config, o.RequestTimeout)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s, err := scheduler.New(ctx, o, apiClient)
	if err != nil {
		log.Fatal(err)
	}
	if err := s.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
