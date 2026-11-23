package app

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"release-management/pkg/syncer"
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	command := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}
	if command != "serve" && command != "plan" && command != "sync" && command != "status" {
		fmt.Fprintln(stderr, "expected serve, plan, sync or status")
		return 2
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFile := flags.String("config", "/etc/release-management/config.yaml", "configuration file")
	binding := flags.String("binding", "", "binding name for plan, sync or status")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || command != "serve" && *binding == "" || command == "serve" && *binding != "" {
		fmt.Fprintln(stderr, "invalid arguments")
		return 2
	}
	config, err := syncer.LoadConfig(*configFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if command != "serve" {
		response, connected, err := localRequest(ctx, config.DataDir, command, *binding)
		if connected {
			if err != nil {
				fmt.Fprintln(stderr, "local command failed")
				return 1
			}
			_ = json.NewEncoder(stdout).Encode(response)
			return response.ExitCode
		}
		if command != "plan" {
			fmt.Fprintln(stderr, "service is not running or control socket is unavailable")
			return 1
		}
		// The directory lock, not a failed socket connection, authorizes standalone cache access.
		unlock, err := syncer.LockDirectory(config.DataDir)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer unlock()
	}
	client, err := syncer.NewGatewayClient(config)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	engine := syncer.NewEngine(config, syncer.NewGitReader(config.DataDir), client)
	if command == "serve" {
		if err := engine.Serve(ctx); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	plan, err := engine.Plan(ctx, *binding)
	response := syncer.ControlResponse{Plan: plan}
	if err != nil {
		response.ExitCode, response.Error = 1, err.Error()
		if failure, ok := err.(*syncer.Failure); ok && failure.Blocked {
			response.ExitCode = 2
		}
	}
	_ = json.NewEncoder(stdout).Encode(response)
	return response.ExitCode
}

func localRequest(ctx context.Context, dir, command, binding string) (syncer.ControlResponse, bool, error) {
	response := syncer.ControlResponse{}
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		return response, false, err
	}
	// Reuse the established connection; a request failure must not fall back to a second standalone operation.
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return connection, nil }, DisableKeepAlives: true}
	defer connection.Close()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 6 * time.Minute}
	method, address := http.MethodPost, "http://local/"+command
	body, _ := json.Marshal(map[string]string{"binding": binding})
	if command == "status" {
		method = http.MethodGet
		address += "?binding=" + url.QueryEscape(binding)
		body = nil
	}
	request, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if err != nil {
		return response, true, err
	}
	request.Header.Set("Content-Type", "application/json")
	result, err := client.Do(request)
	if err != nil {
		return response, true, err
	}
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK && result.StatusCode != http.StatusAccepted {
		return response, true, fmt.Errorf("local command rejected")
	}
	err = json.NewDecoder(io.LimitReader(result.Body, 32<<20)).Decode(&response)
	return response, true, err
}
