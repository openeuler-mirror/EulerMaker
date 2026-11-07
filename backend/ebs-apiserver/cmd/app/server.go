package app

import (
	"github.com/spf13/cobra"

	"ebs-apiserver/cmd/mapping"
	"ebs-apiserver/pkg/server"
)

// NewCommand creates the root command and registers maintenance subcommands.
func NewCommand() *cobra.Command {
	opts := server.NewEulerMakerServerOptions()
	cmd := &cobra.Command{
		Use: "ebs-apiserver", Short: "Run the EulerMaker API server or maintain its ES mappings",
		Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error { return opts.Run(cmd.Context().Done()) },
	}
	opts.AddFlags(cmd.Flags())
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(mapping.NewCommand())
	return cmd
}
