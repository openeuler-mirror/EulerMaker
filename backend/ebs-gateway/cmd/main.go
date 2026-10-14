package main

import (
	"log"

	"ebs-gateway/cmd/app"
)

func main() {
	if err := app.NewServerCommand().Execute(); err != nil {
		log.Fatal(err)
	}
}
