package main

import (
	"log"
	"os"

	"ebs-gateway/pkg/gateway"
)

func main() {
	cfg, err := gateway.LoadConfig(os.Args[1:])
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	srv, err := gateway.NewServer(cfg)
	if err != nil {
		log.Fatalf("create server: %v", err)
	}

	log.Printf("ebs-gateway listening on :%d, upstream=%s", cfg.Port, cfg.APIServerAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("ebs-gateway failed: %v", err)
	}
}
