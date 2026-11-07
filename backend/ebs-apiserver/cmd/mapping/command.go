package mapping

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"ebs-apiserver/pkg/storage/es"
)

var errMappingChanges = errors.New("mapping additions are available; use --apply to update")

// ExitCode distinguishes preview changes from failures and successful commands.
func ExitCode(err error) int {
	if errors.Is(err, errMappingChanges) {
		return 2
	}
	if err != nil {
		return 1
	}
	return 0
}

// NewCommand creates the ES mapping maintenance subcommand.
func NewCommand() *cobra.Command {
	config := es.DefaultConfig()
	var address, keyFile, resources string
	var apply bool
	cmd := &cobra.Command{
		Use: "mapping", Short: "Compare installed ES mappings and optionally add missing fields",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var selected []string
			if cmd.Flags().Changed("resources") {
				selected = strings.Split(resources, ",")
			}
			config.Addresses = []string{address}
			if err := config.LoadAPIKeyFile(keyFile); err != nil {
				return err
			}
			client, err := config.NewConnection()
			if err != nil {
				return err
			}
			report, err := client.UpdateMappings(cmd.Context(), selected, apply)
			if outputErr := printMappingReport(cmd.OutOrStdout(), report); outputErr != nil {
				return outputErr
			}
			if err != nil {
				return err
			}
			if !apply && report.HasAdditions() {
				return errMappingChanges
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&address, "es-servers", config.Addresses[0], "Elasticsearch server address")
	cmd.Flags().StringVar(&keyFile, "es-api-key-file", "", "file containing the encoded Elasticsearch API key")
	cmd.Flags().StringVar(&resources, "resources", "", "comma-separated resource keys; default: all known deployed resources")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply missing mapping fields after preflight succeeds")
	return cmd
}

func printMappingReport(w io.Writer, report es.MappingReport) error {
	var output strings.Builder
	counts := map[string]int{}
	for _, result := range report.Results {
		counts[result.Status]++
		fmt.Fprintf(&output, "%s alias=%s index=%s %s\n", result.Resource, result.Alias, result.Index, result.Status)
		for _, change := range result.Changes {
			before, _ := json.Marshal(change.Before)
			after, _ := json.Marshal(change.After)
			fmt.Fprintf(&output, "  %s %s: %s -> %s\n", change.Kind, change.Path, before, after)
		}
		if result.Message != "" {
			fmt.Fprintf(&output, "  %s\n", result.Message)
		}
	}
	statuses := make([]string, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	fmt.Fprintf(&output, "Summary: resources=%d", len(report.Results))
	for _, status := range statuses {
		fmt.Fprintf(&output, " %s=%d", status, counts[status])
	}
	fmt.Fprintln(&output)
	_, err := io.WriteString(w, output.String())
	return err
}
