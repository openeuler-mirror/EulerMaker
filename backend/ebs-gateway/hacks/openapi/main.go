//go:build ignore

// Run with bash backend/ebs-gateway/hacks/update-openapi.sh [--check].
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

type object = map[string]any

func main() {
	output := flag.String("output", "openapi.json", "generated document path")
	check := flag.Bool("check", false, "check the committed document without writing")
	flag.Parse()
	document := object{
		"openapi": "3.0.3",
		"info": object{
			"title": "EulerMaker Gateway API", "version": "v1",
			"description": "Public Gateway contract. Visibility in this document does not grant access. " +
				"Resource permissions are checked on every request. Field-selected responses may omit properties. " +
				"After an ambiguous write failure, GET the latest object and confirm the intended result before retrying; " +
				"never replay an old decision with a newly substituted resourceVersion.",
		},
		"servers": []object{{"url": "/"}},
		"paths":   operations(),
		"components": object{
			"schemas":   schemas(),
			"responses": errorResponses(),
			"securitySchemes": object{
				"BearerAuth": object{"type": "http", "scheme": "bearer", "bearerFormat": "JWT",
					"description": "User or Runner token; scopes and object ownership are enforced separately."},
				"MachineBasic": object{"type": "http", "scheme": "basic",
					"description": "MachineAccount client ID and client secret, used only to obtain a Runner token."},
			},
		},
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		fail(err)
	}
	data = append(data, '\n')
	if *check {
		existing, err := os.ReadFile(*output)
		if err != nil {
			fail(err)
		}
		if !bytes.Equal(existing, data) {
			fail(fmt.Errorf("OpenAPI document is stale; run bash backend/ebs-gateway/hacks/update-openapi.sh"))
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0755); err != nil {
		fail(err)
	}
	if err := os.WriteFile(*output, data, 0644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
