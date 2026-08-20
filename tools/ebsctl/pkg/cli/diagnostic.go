package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ebsctl/pkg/client"
	"ebsctl/pkg/printer"
	"ebsctl/pkg/resource"
	"github.com/spf13/cobra"
)

func (a *App) describeCommand() *cobra.Command {
	return &cobra.Command{
		Use: "describe RESOURCE NAME", Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 {
				return UsageError("usage: ebsctl describe RESOURCE NAME")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			definition, api, path, err := a.namedResource(args[0], args[1], false)
			if err != nil {
				return err
			}
			body, _, err := api.Do(cmd.Context(), http.MethodGet, path, "", nil, definition.Singular, args[1])
			if err != nil {
				return err
			}
			var object map[string]any
			decoder := json.NewDecoder(strings.NewReader(string(body)))
			decoder.UseNumber()
			if err := decoder.Decode(&object); err != nil {
				return fmt.Errorf("decode object: %w", err)
			}
			return printer.Describe(a.streams.Out, definition, object)
		},
	}
}

func (a *App) waitCommand() *cobra.Command {
	var condition string
	var timeout time.Duration
	command := &cobra.Command{
		Use: "wait RESOURCE NAME --for=CONDITION", Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 || condition == "" {
				return UsageError("usage: ebsctl wait RESOURCE NAME --for=CONDITION")
			}
			if _, err := parseWaitCondition(condition); err != nil {
				return UsageError("%v", err)
			}
			if timeout <= 0 {
				return UsageError("--timeout must be greater than zero")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			definition, api, path, err := a.namedResource(args[0], args[1], true)
			if err != nil {
				return err
			}
			match, err := parseWaitCondition(condition)
			if err != nil {
				return UsageError("%v", err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return a.waitFor(ctx, api, definition, args[1], path, match)
		},
	}
	command.Flags().StringVar(&condition, "for", "", "condition=TYPE, delete, or jsonpath='{.path}'=value")
	command.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "wait timeout")
	return command
}

func (a *App) waitFor(ctx context.Context, api *client.Client, definition resource.Definition, name, objectPath string, match waitCondition) error {
	collection, err := definition.CollectionPath(projectFromPath(objectPath, definition))
	if err != nil {
		return err
	}
	resourceVersion := ""
	for {
		body, _, getErr := api.Do(ctx, http.MethodGet, objectPath, "", nil, definition.Singular, name)
		if getErr != nil {
			var apiError *client.APIError
			if match.deleted && errors.As(getErr, &apiError) && apiError.StatusCode == http.StatusNotFound {
				fmt.Fprintf(a.streams.Out, "%s/%s deleted\n", definition.Singular, name)
				return nil
			}
			return getErr
		}
		var current map[string]any
		if err := json.Unmarshal(body, &current); err != nil {
			return fmt.Errorf("decode object: %w", err)
		}
		if !match.deleted && match.matches(current) {
			fmt.Fprintf(a.streams.Out, "%s/%s condition met\n", definition.Singular, name)
			return nil
		}
		resourceVersion = client.ResourceVersion(current)

		for {
			query := url.Values{"watch": {"true"}, "fieldSelector": {"metadata.name=" + name}, "allowWatchBookmarks": {"true"}, "timeoutSeconds": {"300"}}
			if resourceVersion != "" {
				query.Set("resourceVersion", resourceVersion)
			}
			response, err := api.OpenWatch(ctx, client.AddQuery(collection, query), definition.Singular)
			if err != nil {
				if ctx.Err() != nil {
					return &ExitError{Code: 1, Err: fmt.Errorf("timed out waiting for %s/%s", definition.Singular, name)}
				}
				var apiError *client.APIError
				if errors.As(err, &apiError) && apiError.StatusCode == http.StatusGone {
					break
				}
				return err
			}
			decoder := json.NewDecoder(response.Body)
			expired := false
			for {
				var event watchEvent
				decodeErr := decoder.Decode(&event)
				if decodeErr != nil {
					response.Body.Close()
					if ctx.Err() != nil {
						return &ExitError{Code: 1, Err: fmt.Errorf("timed out waiting for %s/%s", definition.Singular, name)}
					}
					if decodeErr == io.EOF || decodeErr == io.ErrUnexpectedEOF {
						break
					}
					return fmt.Errorf("decode watch event: %w", decodeErr)
				}
				if candidate := client.ResourceVersion(event.Object); candidate != "" {
					resourceVersion = candidate
				}
				if event.Type == "ERROR" && int(number(event.Object["code"])) == http.StatusGone {
					expired = true
					break
				}
				if match.deleted && event.Type == "DELETED" || !match.deleted && match.matches(event.Object) {
					response.Body.Close()
					fmt.Fprintf(a.streams.Out, "%s/%s condition met\n", definition.Singular, name)
					return nil
				}
			}
			response.Body.Close()
			if expired {
				break
			}
		}
	}
}

type waitCondition struct {
	deleted bool
	path    []string
	value   string
}

func parseWaitCondition(value string) (waitCondition, error) {
	if value == "delete" {
		return waitCondition{deleted: true}, nil
	}
	if strings.HasPrefix(value, "condition=") {
		typeName := strings.TrimPrefix(value, "condition=")
		if typeName == "" {
			return waitCondition{}, fmt.Errorf("condition type is required")
		}
		return waitCondition{path: []string{"status", "conditions"}, value: typeName}, nil
	}
	if strings.HasPrefix(value, "jsonpath=") {
		expression := strings.TrimPrefix(value, "jsonpath=")
		closing := strings.Index(expression, "}")
		if closing < 0 {
			return waitCondition{}, fmt.Errorf("JSONPath condition must be jsonpath='{.field}'=value")
		}
		path := strings.Trim(expression[:closing+1], "'\"")
		remainder := strings.TrimLeft(expression[closing+1:], "'\"")
		if !strings.HasPrefix(remainder, "=") {
			return waitCondition{}, fmt.Errorf("JSONPath condition must be jsonpath='{.field}'=value")
		}
		expected := strings.TrimPrefix(remainder, "=")
		if !strings.HasPrefix(path, "{.") || !strings.HasSuffix(path, "}") || expected == "" {
			return waitCondition{}, fmt.Errorf("unsupported JSONPath condition")
		}
		segments := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "{."), "}"), ".")
		for _, segment := range segments {
			if segment == "" || strings.ContainsAny(segment, "[]*?") {
				return waitCondition{}, fmt.Errorf("only simple field JSONPath is supported")
			}
		}
		return waitCondition{path: segments, value: expected}, nil
	}
	return waitCondition{}, fmt.Errorf("unsupported wait condition %q", value)
}

func (condition waitCondition) matches(object map[string]any) bool {
	if len(condition.path) == 2 && condition.path[0] == "status" && condition.path[1] == "conditions" {
		status, _ := object["status"].(map[string]any)
		conditions, _ := status["conditions"].([]any)
		for _, value := range conditions {
			mapped, _ := value.(map[string]any)
			if fmt.Sprint(mapped["type"]) == condition.value && strings.EqualFold(fmt.Sprint(mapped["status"]), "true") {
				return true
			}
		}
		return false
	}
	var current any = object
	for _, segment := range condition.path {
		mapped, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current = mapped[segment]
	}
	return fmt.Sprint(current) == condition.value
}

func projectFromPath(path string, definition resource.Definition) string {
	if !definition.Namespaced {
		return ""
	}
	parts := strings.Split(path, "/")
	for index, part := range parts {
		if part == "projects" && index+1 < len(parts) {
			return parts[index+1]
		}
	}
	return ""
}

func (a *App) versionCommand() *cobra.Command {
	var server bool
	command := &cobra.Command{
		Use: "version", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(a.streams.Out, "Client Version: %s\n", Version)
			if !server {
				return nil
			}
			_, resolved, err := a.loadResolved(true)
			if err != nil {
				return err
			}
			api, err := a.client(resolved, false)
			if err != nil {
				return err
			}
			if _, _, err := api.Do(cmd.Context(), http.MethodGet, "/healthz", "", nil, "gateway", ""); err != nil {
				return err
			}
			fmt.Fprintln(a.streams.Out, "Server: reachable (version endpoint unavailable)")
			return nil
		},
	}
	command.Flags().BoolVar(&server, "server", false, "check Gateway connectivity")
	return command
}
