package main

import (
	"log"

	"artifact-manager/cmd/app"
)

func main() {
	if err := app.NewServerCommand().Execute(); err != nil {
		log.Fatal(err)
	}
}
