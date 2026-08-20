package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"ebsctl/pkg/client"
	configpkg "ebsctl/pkg/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (a *App) loginCommand() *cobra.Command {
	var username, contextName, caFile, serverName string
	var passwordStdin bool
	command := &cobra.Command{
		Use:   "login GATEWAY --username USER",
		Short: "Log in and save an access token",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 || username == "" {
				return UsageError("usage: ebsctl login GATEWAY --username USER")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			gateway := strings.TrimSuffix(args[0], "/")
			if err := configpkg.ValidateGateway(gateway); err != nil {
				return UsageError("%v", err)
			}
			var password []byte
			var err error
			if passwordStdin {
				password, err = configpkg.ReadAll(a.streams.In, 4096)
				password = []byte(strings.TrimSuffix(strings.TrimSuffix(string(password), "\n"), "\r"))
			} else {
				file := stdinFile()
				if !term.IsTerminal(int(file.Fd())) {
					return UsageError("stdin is not a terminal; use --password-stdin")
				}
				fmt.Fprint(a.streams.ErrOut, "Password: ")
				password, err = term.ReadPassword(int(file.Fd()))
				fmt.Fprintln(a.streams.ErrOut)
			}
			if err != nil {
				return UsageError("read password: %v", err)
			}
			api, err := client.New(client.Options{Gateway: gateway, CAFile: caFile, ServerName: serverName, InsecureSkipVerify: a.insecure, Timeout: a.timeout, Verbose: a.verbose, Diagnostic: a.streams.ErrOut})
			if err != nil {
				return UsageError("%v", err)
			}
			payload, _ := json.Marshal(map[string]string{"username": username, "password": string(password)})
			for index := range password {
				password[index] = 0
			}
			body, _, err := api.Do(cmd.Context(), http.MethodPost, "/auth/login", "application/json", payload, "user", username)
			if err != nil {
				return err
			}
			var response struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(body, &response); err != nil || response.Token == "" {
				return fmt.Errorf("Gateway returned an invalid login response")
			}
			configuration, err := configpkg.Load(a.configPath)
			if err != nil {
				return UsageError("%v", err)
			}
			if contextName == "" {
				contextName = configpkg.ContextName(gateway)
			}
			current := configuration.Contexts[contextName]
			current.Gateway, current.User = gateway, username
			current.TLS = configpkg.TLSConfig{CAFile: caFile, ServerName: serverName}
			configuration.Contexts[contextName] = current
			configuration.Credentials[contextName] = configpkg.Credential{Token: response.Token}
			configuration.CurrentContext = contextName
			if err := configpkg.Save(a.configPath, configuration); err != nil {
				return UsageError("save config: %v", err)
			}
			fmt.Fprintf(a.streams.Out, "Logged in to %s as %s (context %s)\n", gateway, username, contextName)
			return nil
		},
	}
	command.Flags().StringVar(&username, "username", "", "username")
	command.Flags().StringVar(&contextName, "context-name", "", "saved context name")
	command.Flags().StringVar(&caFile, "certificate-authority", "", "Gateway CA certificate file")
	command.Flags().StringVar(&serverName, "tls-server-name", "", "Gateway TLS server name")
	command.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read password from stdin")
	return command
}

func (a *App) logoutCommand() *cobra.Command {
	return &cobra.Command{
		Use: "logout", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			configuration, resolved, err := a.loadResolved(false)
			if err != nil {
				return err
			}
			if resolved.Name == "" {
				return UsageError("no current context")
			}
			delete(configuration.Credentials, resolved.Name)
			if err := configpkg.Save(a.configPath, configuration); err != nil {
				return UsageError("save config: %v", err)
			}
			fmt.Fprintf(a.streams.Out, "Logged out of context %s\n", resolved.Name)
			return nil
		},
	}
}

func (a *App) configCommand() *cobra.Command {
	command := &cobra.Command{Use: "config", Short: "Manage contexts"}
	command.AddCommand(
		&cobra.Command{Use: "get-contexts", Args: cobra.NoArgs, RunE: a.getContexts},
		&cobra.Command{Use: "use-context NAME", Args: cobra.ExactArgs(1), RunE: a.useContext},
		&cobra.Command{Use: "set-project PROJECT", Args: cobra.ExactArgs(1), RunE: a.setProject},
	)
	return command
}

func (a *App) getContexts(cmd *cobra.Command, args []string) error {
	configuration, err := configpkg.Load(a.configPath)
	if err != nil {
		return UsageError("%v", err)
	}
	names := make([]string, 0, len(configuration.Contexts))
	for name := range configuration.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintln(a.streams.Out, "CURRENT\tNAME\tGATEWAY\tUSER\tPROJECT")
	for _, name := range names {
		current := ""
		if name == configuration.CurrentContext {
			current = "*"
		}
		value := configuration.Contexts[name]
		fmt.Fprintf(a.streams.Out, "%s\t%s\t%s\t%s\t%s\n", current, name, value.Gateway, value.User, value.Project)
	}
	return nil
}

func (a *App) useContext(cmd *cobra.Command, args []string) error {
	configuration, err := configpkg.Load(a.configPath)
	if err != nil {
		return UsageError("%v", err)
	}
	if _, exists := configuration.Contexts[args[0]]; !exists {
		return UsageError("context %q does not exist", args[0])
	}
	configuration.CurrentContext = args[0]
	if err := configpkg.Save(a.configPath, configuration); err != nil {
		return UsageError("save config: %v", err)
	}
	fmt.Fprintf(a.streams.Out, "Switched to context %s\n", args[0])
	return nil
}

func (a *App) setProject(cmd *cobra.Command, args []string) error {
	configuration, resolved, err := a.loadResolved(false)
	if err != nil {
		return err
	}
	if resolved.Name == "" {
		return UsageError("no current context")
	}
	value, exists := configuration.Contexts[resolved.Name]
	if !exists {
		return UsageError("context %q does not exist", resolved.Name)
	}
	value.Project = args[0]
	configuration.Contexts[resolved.Name] = value
	if err := configpkg.Save(a.configPath, configuration); err != nil {
		return UsageError("save config: %v", err)
	}
	fmt.Fprintf(a.streams.Out, "Context %s now uses Project %s\n", resolved.Name, args[0])
	return nil
}
