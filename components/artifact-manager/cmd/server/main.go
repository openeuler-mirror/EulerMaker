package main

import (
	artifact "artifact-manager/pkg/artifact"
	"log"
	"os"
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
	log.Fatal(s.ListenAndServe())
}
