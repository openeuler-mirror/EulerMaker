package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"ebsctl/pkg/client"
	"ebsctl/pkg/printer"
	"ebsctl/pkg/resource"
	"github.com/spf13/cobra"
)

type outputFlags struct {
	format    string
	noHeaders bool
}

func addOutputFlags(command *cobra.Command, flags *outputFlags) {
	command.Flags().StringVarP(&flags.format, "output", "o", "table", "output format: table, wide, yaml, json, name")
	command.Flags().BoolVar(&flags.noHeaders, "no-headers", false, "omit table headers")
}

func validateOutput(value string) error {
	switch value {
	case "table", "wide", "yaml", "json", "name":
		return nil
	default:
		return UsageError("unsupported output format %q", value)
	}
}

func (a *App) getCommand() *cobra.Command {
	var output outputFlags
	var selector, fieldSelector, continueToken string
	var limit int
	var watch, watchOnly, mine bool
	command := &cobra.Command{
		Use: "get RESOURCE [NAME]", Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return UsageError("usage: ebsctl get RESOURCE [NAME]")
			}
			return validateOutput(output.format)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if watchOnly {
				watch = true
			}
			definition, err := resource.Resolve(args[0])
			if err != nil {
				return UsageError("%v", err)
			}
			if mine && (definition.Kind != "Project" || len(args) != 1 || watch) {
				return UsageError("--mine is only supported by non-watch 'get projects'")
			}
			_, resolved, err := a.loadResolved(true)
			if err != nil {
				return err
			}
			api, err := a.client(resolved, watch || mine)
			if err != nil {
				return err
			}
			mineUser := ""
			if mine {
				identity, err := api.CheckIdentity(cmd.Context())
				if err != nil {
					return err
				}
				if identity.Type != "user" && identity.Type != "admin" && identity.Type != "ops" {
					return &ExitError{Code: 3, Err: fmt.Errorf("--mine requires a user identity")}
				}
				mineUser = identity.Name
			}
			path, err := definition.CollectionPath(resolved.Project)
			if len(args) == 2 && err == nil {
				path, err = definition.ObjectPath(resolved.Project, args[1])
			}
			if err != nil {
				return UsageError("%v", err)
			}
			query := url.Values{}
			if selector != "" {
				query.Set("labelSelector", selector)
			}
			if fieldSelector != "" {
				query.Set("fieldSelector", fieldSelector)
			}
			if limit < 0 {
				return UsageError("--limit cannot be negative")
			}
			if limit > 0 {
				query.Set("limit", fmt.Sprint(limit))
			}
			if continueToken != "" {
				query.Set("continue", continueToken)
			}
			if watch {
				if len(args) == 2 {
					return UsageError("--watch requires a resource collection")
				}
				return a.runWatch(cmd.Context(), api, definition, path, query, output, watchOnly)
			}
			body, _, err := api.Do(cmd.Context(), http.MethodGet, client.AddQuery(path, query), "", nil, definition.Singular, objectName(args))
			if err != nil {
				return err
			}
			if mine {
				body, err = filterProjectsForUser(body, mineUser)
				if err != nil {
					return err
				}
			}
			return printer.New(a.streams.Out, printer.Options{Format: output.format, NoHeaders: output.noHeaders}).Print(definition, body)
		},
	}
	addOutputFlags(command, &output)
	command.Flags().StringVarP(&selector, "selector", "l", "", "label selector")
	command.Flags().StringVar(&fieldSelector, "field-selector", "", "field selector")
	command.Flags().IntVar(&limit, "limit", 0, "page size")
	command.Flags().StringVar(&continueToken, "continue", "", "pagination continuation token")
	command.Flags().BoolVarP(&watch, "watch", "w", false, "watch for changes")
	command.Flags().BoolVar(&watchOnly, "watch-only", false, "do not print the initial list")
	command.Flags().BoolVar(&mine, "mine", false, "show Projects owned by or shared with the current user")
	return command
}

func filterProjectsForUser(data []byte, username string) ([]byte, error) {
	var list map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&list); err != nil {
		return nil, fmt.Errorf("decode ProjectList: %w", err)
	}
	items, ok := list["items"].([]any)
	if !ok {
		return nil, fmt.Errorf("decode ProjectList: items are required")
	}
	filtered := make([]any, 0, len(items))
	for _, item := range items {
		project, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("decode ProjectList: invalid item")
		}
		metadata, _ := project["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		if fmt.Sprint(labels["ebs.io/owner-user"]) == username || fmt.Sprint(labels["ebs.io/member-user."+username]) == "true" {
			filtered = append(filtered, project)
		}
	}
	list["items"] = filtered
	result, err := json.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("encode filtered ProjectList: %w", err)
	}
	return result, nil
}

func (a *App) createCommand() *cobra.Command  { return a.manifestCommand("create", http.MethodPost) }
func (a *App) replaceCommand() *cobra.Command { return a.manifestCommand("replace", http.MethodPut) }

func (a *App) manifestCommand(name, method string) *cobra.Command {
	var file string
	var validate, failFast bool
	var output outputFlags
	command := &cobra.Command{
		Use: name + " -f FILE", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return UsageError("-f is required")
			}
			if err := validateOutput(output.format); err != nil {
				return err
			}
			_, resolved, err := a.loadResolved(true)
			if err != nil {
				return err
			}
			api, err := a.client(resolved, true)
			if err != nil {
				return err
			}
			manifests, err := resource.ReadManifests(file, resolved.Project, validate, a.streams.In)
			if err != nil {
				return UsageError("%v", err)
			}
			failures := 0
			for _, manifest := range manifests {
				if method == http.MethodPut && !resource.HasResourceVersion(manifest) {
					err = UsageError("%s/%s: replace requires metadata.resourceVersion", manifest.Definition.Singular, manifest.Name)
				} else {
					path, pathErr := manifest.Definition.CollectionPath(manifest.Project)
					if method == http.MethodPut && pathErr == nil {
						path, pathErr = manifest.Definition.ObjectPath(manifest.Project, manifest.Name)
					}
					if pathErr != nil {
						err = pathErr
					} else {
						var body []byte
						body, _, err = api.Do(cmd.Context(), method, path, "application/json", manifest.Data, manifest.Definition.Singular, manifest.Name)
						if err == nil {
							err = printer.New(a.streams.Out, printer.Options{Format: output.format, NoHeaders: output.noHeaders}).Print(manifest.Definition, body)
						}
					}
				}
				if err != nil {
					failures++
					fmt.Fprintln(a.streams.ErrOut, "error:", err)
					if failFast {
						break
					}
				}
			}
			if failures > 0 {
				return &ExitError{Code: 1, Err: fmt.Errorf("%d object(s) failed", failures)}
			}
			return nil
		},
	}
	command.Flags().StringVarP(&file, "filename", "f", "", "file, directory, or - for stdin")
	command.Flags().BoolVar(&validate, "validate", true, "validate input fields locally")
	command.Flags().BoolVar(&failFast, "fail-fast", false, "stop after the first failure")
	addOutputFlags(command, &output)
	return command
}

func (a *App) patchCommand() *cobra.Command {
	var patch, patchType string
	var output outputFlags
	command := &cobra.Command{
		Use: "patch RESOURCE NAME --patch JSON", Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 || patch == "" {
				return UsageError("usage: ebsctl patch RESOURCE NAME --patch JSON")
			}
			if patchType != "merge" {
				return UsageError("only --type=merge is supported")
			}
			if !json.Valid([]byte(patch)) {
				return UsageError("patch is not valid JSON")
			}
			return validateOutput(output.format)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			definition, api, path, err := a.namedResource(args[0], args[1], true)
			if err != nil {
				return err
			}
			body, _, err := api.Do(cmd.Context(), http.MethodPatch, path, "application/merge-patch+json", []byte(patch), definition.Singular, args[1])
			if err != nil {
				return err
			}
			return printer.New(a.streams.Out, printer.Options{Format: output.format, NoHeaders: output.noHeaders}).Print(definition, body)
		},
	}
	command.Flags().StringVar(&patch, "patch", "", "merge patch JSON")
	command.Flags().StringVar(&patchType, "type", "merge", "patch type (merge only)")
	addOutputFlags(command, &output)
	return command
}

func (a *App) deleteCommand() *cobra.Command {
	var all, yes bool
	command := &cobra.Command{
		Use: "delete RESOURCE [NAME]", Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 || (len(args) == 1) == !all {
				return UsageError("specify exactly one NAME, or use --all")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			definition, err := resource.Resolve(args[0])
			if err != nil {
				return UsageError("%v", err)
			}
			_, resolved, err := a.loadResolved(true)
			if err != nil {
				return err
			}
			api, err := a.client(resolved, true)
			if err != nil {
				return err
			}
			if all {
				if !yes {
					fmt.Fprintf(a.streams.ErrOut, "Delete all %s in the selected scope? [y/N] ", definition.Plural)
					answer, _ := bufio.NewReader(a.streams.In).ReadString('\n')
					if strings.ToLower(strings.TrimSpace(answer)) != "y" && strings.ToLower(strings.TrimSpace(answer)) != "yes" {
						fmt.Fprintln(a.streams.ErrOut, "Cancelled")
						return nil
					}
				}
				path, err := definition.CollectionPath(resolved.Project)
				if err != nil {
					return UsageError("%v", err)
				}
				list, _, err := api.Do(cmd.Context(), http.MethodGet, path, "", nil, definition.Singular, "")
				if err != nil {
					return err
				}
				var value struct {
					Items []json.RawMessage `json:"items"`
				}
				if err := json.Unmarshal(list, &value); err != nil {
					return fmt.Errorf("decode list: %w", err)
				}
				failures := 0
				for _, raw := range value.Items {
					var object struct {
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
					}
					if json.Unmarshal(raw, &object) != nil || object.Metadata.Name == "" {
						failures++
						continue
					}
					objectPath, _ := definition.ObjectPath(resolved.Project, object.Metadata.Name)
					if _, _, err := api.Do(cmd.Context(), http.MethodDelete, objectPath, "", nil, definition.Singular, object.Metadata.Name); err != nil {
						failures++
						fmt.Fprintln(a.streams.ErrOut, "error:", err)
					} else {
						fmt.Fprintf(a.streams.Out, "%s/%s deleted\n", definition.Singular, object.Metadata.Name)
					}
				}
				if failures > 0 {
					return &ExitError{Code: 1, Err: fmt.Errorf("%d object(s) failed", failures)}
				}
				return nil
			}
			path, err := definition.ObjectPath(resolved.Project, args[1])
			if err != nil {
				return UsageError("%v", err)
			}
			if _, _, err := api.Do(cmd.Context(), http.MethodDelete, path, "", nil, definition.Singular, args[1]); err != nil {
				return err
			}
			fmt.Fprintf(a.streams.Out, "%s/%s deleted\n", definition.Singular, args[1])
			return nil
		},
	}
	command.Flags().BoolVar(&all, "all", false, "delete all resources in the selected scope")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "skip bulk-delete confirmation")
	return command
}

func (a *App) namedResource(resourceName, name string, token bool) (resource.Definition, *client.Client, string, error) {
	definition, err := resource.Resolve(resourceName)
	if err != nil {
		return resource.Definition{}, nil, "", UsageError("%v", err)
	}
	_, resolved, err := a.loadResolved(true)
	if err != nil {
		return resource.Definition{}, nil, "", err
	}
	api, err := a.client(resolved, token)
	if err != nil {
		return resource.Definition{}, nil, "", err
	}
	path, err := definition.ObjectPath(resolved.Project, name)
	if err != nil {
		return resource.Definition{}, nil, "", UsageError("%v", err)
	}
	return definition, api, path, nil
}

func objectName(args []string) string {
	if len(args) == 2 {
		return args[1]
	}
	return ""
}
