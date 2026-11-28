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
				"GET /healthz directly returns a fixed plain-text response without middleware; business requests retain recovery and audit. " +
				"Authentication is required by default. Public resource reads allow anonymous requests but validate any supplied token; invalid tokens never fall back to anonymous access. " +
				"Except for GET /healthz, requests that pass authentication are rate-limited by authenticated identity/IP or anonymous IP. Rate limits return 429 with Retry-After. Local registration uses only the global anonymous limit. Dedicated local login limits are keyed only by username, refilling at 5 requests per minute with a capacity of 5. Machine-token limits are keyed only by clientID, refilling at 10 requests per minute with a capacity of 10. OAuth and watch connection limits also apply. " +
				"Watch is allowed only on GET /apis/ebs/v1/projects/{project}/jobs and GET /apis/ebs/v1/runners/{runner}/jobs; " +
				"other matched routes reject Watch with 403 before authentication. Allowed Watch requests still require authentication and authorization. " +
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
					"description": "User, Runner or service token. Service resource requests require both an exact Project match and the corresponding projects:update, builds:create or builds:abort permission from the current MachineAccount. Empty grants deny resource access; use anonymous requests for public reads."},
				"MachineBasic": object{"type": "http", "scheme": "basic",
					"description": "MachineAccount client ID and client secret for /auth/machine-token. The stored account type selects Runner or service identity."},
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
