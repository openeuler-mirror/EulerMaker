package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"ebsctl/pkg/client"
	configpkg "ebsctl/pkg/config"
)

func (a *App) loginCommand() *cobra.Command {
	var contextName, caFile, serverName string
	var tokenStdin bool
	command := &cobra.Command{
		Use:   "login GATEWAY --token-stdin",
		Short: "Verify and save a personal access credential from stdin",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 || !tokenStdin {
				return UsageError("usage: ebsctl login GATEWAY --token-stdin")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			gateway := strings.TrimSuffix(args[0], "/")
			if err := configpkg.ValidateGateway(gateway); err != nil {
				return UsageError("%v", err)
			}
			if !strings.HasPrefix(gateway, "https://") || a.insecure {
				return UsageError("credential login requires HTTPS with certificate verification")
			}
			if a.streams.In == nil {
				return UsageError("token stdin is required")
			}
			input, err := configpkg.ReadAll(a.streams.In, 4096)
			if err != nil {
				return UsageError("cannot read token: expected at most 4096 bytes from stdin")
			}
			token := strings.TrimSuffix(strings.TrimSuffix(string(input), "\n"), "\r")
			clear(input)
			if token == "" {
				return UsageError("token stdin must contain one nonempty credential")
			}
			api, err := client.New(client.Options{
				Gateway: gateway, Token: token, CAFile: caFile, ServerName: serverName,
				Timeout: a.timeout, Verbose: a.verbose, Diagnostic: a.streams.ErrOut,
			})
			if err != nil {
				return UsageError("%v", err)
			}
			identity, err := api.CheckIdentity(cmd.Context())
			if err != nil {
				return err
			}
			configuration, err := configpkg.Load(a.configPath)
			if err != nil {
				return UsageError("%v", err)
			}
			if contextName == "" {
				contextName = configpkg.ContextName(gateway)
			}
			current := configuration.Contexts[contextName]
			current.Gateway, current.User = gateway, identity.Name
			current.TLS = configpkg.TLSConfig{CAFile: caFile, ServerName: serverName}
			configuration.Contexts[contextName] = current
			configuration.Credentials[contextName] = configpkg.Credential{Token: token}
			configuration.CurrentContext = contextName
			if err := configpkg.Save(a.configPath, configuration); err != nil {
				return UsageError("save config: %v", err)
			}
			fmt.Fprintf(a.streams.Out, "Logged in to %s as %s (context %s)\n", gateway, identity.Name, contextName)
			return nil
		},
	}
	command.Flags().StringVar(&contextName, "context-name", "", "saved context name")
	command.Flags().StringVar(&caFile, "certificate-authority", "", "Gateway CA certificate file")
	command.Flags().StringVar(&serverName, "tls-server-name", "", "Gateway TLS server name")
	command.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read a personal access credential (or User JWT) from stdin")
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
