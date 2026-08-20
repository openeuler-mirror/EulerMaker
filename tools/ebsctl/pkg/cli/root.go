package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"ebsctl/pkg/client"
	configpkg "ebsctl/pkg/config"
	"github.com/spf13/cobra"
)

const Version = "0.1.0"

type Streams struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
}

type App struct {
	streams    Streams
	configPath string
	context    string
	project    string
	gateway    string
	timeout    time.Duration
	insecure   bool
	verbose    bool
}

type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

func UsageError(format string, values ...any) error {
	return &ExitError{Code: 2, Err: fmt.Errorf(format, values...)}
}

func NewCommand(streams Streams) (*cobra.Command, *App) {
	app := &App{streams: streams, timeout: 30 * time.Second}
	command := &cobra.Command{
		Use:               "ebsctl",
		Short:             "Manage EulerMaker resources",
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	defaultPath, err := configpkg.DefaultPath()
	if err == nil {
		app.configPath = defaultPath
	}
	flags := command.PersistentFlags()
	flags.StringVar(&app.configPath, "config", app.configPath, "configuration file")
	flags.StringVar(&app.context, "context", "", "context override")
	flags.StringVarP(&app.project, "project", "p", "", "Project override")
	flags.StringVar(&app.gateway, "gateway", "", "Gateway URL override")
	flags.DurationVar(&app.timeout, "request-timeout", app.timeout, "ordinary request timeout")
	flags.BoolVar(&app.insecure, "insecure-skip-tls-verify", false, "skip Gateway TLS verification")
	flags.BoolVar(&app.verbose, "verbose", false, "print request diagnostics to stderr")

	command.AddCommand(
		app.loginCommand(), app.logoutCommand(), app.configCommand(),
		app.getCommand(), app.createCommand(), app.replaceCommand(), app.patchCommand(), app.deleteCommand(),
		app.describeCommand(), app.waitCommand(), app.versionCommand(),
	)
	return command, app
}

func Execute(ctx context.Context, streams Streams, args []string) int {
	command, _ := NewCommand(streams)
	command.SetArgs(args)
	err := command.ExecuteContext(ctx)
	if err == nil || errors.Is(err, context.Canceled) {
		return 0
	}
	fmt.Fprintln(streams.ErrOut, "error:", err)
	var exitError *ExitError
	if errors.As(err, &exitError) {
		return exitError.Code
	}
	return client.ExitClass(err)
}

func (a *App) loadResolved(requireGateway bool) (configpkg.Config, configpkg.Resolved, error) {
	if a.configPath == "" {
		return configpkg.Config{}, configpkg.Resolved{}, UsageError("--config is required when the home directory cannot be resolved")
	}
	configuration, err := configpkg.Load(a.configPath)
	if err != nil {
		return configpkg.Config{}, configpkg.Resolved{}, UsageError("%v", err)
	}
	resolved, err := configpkg.Resolve(configuration, a.configPath, a.context, a.gateway, a.project)
	if err != nil {
		return configpkg.Config{}, configpkg.Resolved{}, UsageError("%v", err)
	}
	if requireGateway && resolved.Gateway == "" {
		return configpkg.Config{}, configpkg.Resolved{}, UsageError("Gateway is not configured; run ebsctl login or use --gateway")
	}
	return configuration, resolved, nil
}

func (a *App) client(resolved configpkg.Resolved, requireToken bool) (*client.Client, error) {
	if requireToken && resolved.Token == "" {
		return nil, &ExitError{Code: 3, Err: fmt.Errorf("not logged in; run ebsctl login or set EBS_TOKEN")}
	}
	if a.insecure {
		fmt.Fprintln(a.streams.ErrOut, "warning: TLS certificate verification is disabled")
	}
	return client.New(client.Options{
		Gateway: resolved.Gateway, Token: resolved.Token, CAFile: resolved.TLS.CAFile, ServerName: resolved.TLS.ServerName,
		InsecureSkipVerify: a.insecure, Timeout: a.timeout, Verbose: a.verbose, Diagnostic: a.streams.ErrOut,
	})
}

func stdinFile() *os.File {
	return os.Stdin
}
