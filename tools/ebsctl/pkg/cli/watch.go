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
)

type watchEvent struct {
	Type   string         `json:"type"`
	Object map[string]any `json:"object"`
}

func (a *App) runWatch(ctx context.Context, api *client.Client, definition resource.Definition, path string, baseQuery url.Values, output outputFlags, watchOnly bool) error {
	resourceVersion := ""
	if !watchOnly {
		body, _, err := api.Do(ctx, http.MethodGet, client.AddQuery(path, baseQuery), "", nil, definition.Singular, "")
		if err != nil {
			return err
		}
		if err := printer.New(a.streams.Out, printer.Options{Format: output.format, NoHeaders: output.noHeaders}).Print(definition, body); err != nil {
			return err
		}
		var list map[string]any
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.UseNumber()
		if decoder.Decode(&list) == nil {
			resourceVersion = client.ResourceVersion(list)
		}
	}
	for {
		query := cloneValues(baseQuery)
		query.Del("limit")
		query.Del("continue")
		query.Set("watch", "true")
		query.Set("allowWatchBookmarks", "true")
		query.Set("timeoutSeconds", "300")
		if resourceVersion != "" {
			query.Set("resourceVersion", resourceVersion)
		}
		response, err := api.OpenWatch(ctx, client.AddQuery(path, query), definition.Singular)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var apiError *client.APIError
			if errors.As(err, &apiError) && apiError.StatusCode == http.StatusGone {
				resourceVersion, err = a.relistForWatch(ctx, api, definition, path, baseQuery, output, watchOnly)
				if err != nil {
					return err
				}
				continue
			}
			return err
		}
		lastVersion, expired, err := a.consumeWatch(response.Body, definition, output, resourceVersion)
		response.Body.Close()
		if lastVersion != "" {
			resourceVersion = lastVersion
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		if expired {
			resourceVersion, err = a.relistForWatch(ctx, api, definition, path, baseQuery, output, true)
			if err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (a *App) consumeWatch(reader io.Reader, definition resource.Definition, output outputFlags, currentVersion string) (string, bool, error) {
	decoder := json.NewDecoder(reader)
	eventPrinter := printer.New(a.streams.Out, printer.Options{Format: output.format, NoHeaders: output.noHeaders})
	version := currentVersion
	for {
		var event watchEvent
		if err := decoder.Decode(&event); err != nil {
			return version, false, err
		}
		if candidate := client.ResourceVersion(event.Object); candidate != "" {
			version = candidate
		}
		if event.Type == "ERROR" {
			code := int(number(event.Object["code"]))
			if code == http.StatusGone {
				return version, true, nil
			}
			return version, false, fmt.Errorf("watch error: %v", event.Object["message"])
		}
		if event.Type == "BOOKMARK" {
			continue
		}
		value := any(event.Object)
		if output.format == "json" || output.format == "yaml" {
			value = map[string]any{"type": event.Type, "object": event.Object}
		}
		if err := eventPrinter.PrintValue(definition, value, event.Type); err != nil {
			return version, false, err
		}
	}
}

func (a *App) relistForWatch(ctx context.Context, api *client.Client, definition resource.Definition, path string, query url.Values, output outputFlags, suppress bool) (string, error) {
	body, _, err := api.Do(ctx, http.MethodGet, client.AddQuery(path, query), "", nil, definition.Singular, "")
	if err != nil {
		return "", err
	}
	if !suppress {
		if err := printer.New(a.streams.Out, printer.Options{Format: output.format, NoHeaders: output.noHeaders}).Print(definition, body); err != nil {
			return "", err
		}
	}
	var list map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&list); err != nil {
		return "", fmt.Errorf("decode relist: %w", err)
	}
	return client.ResourceVersion(list), nil
}

func cloneValues(input url.Values) url.Values {
	result := url.Values{}
	for key, values := range input {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func number(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		result, _ := typed.Int64()
		return result
	case float64:
		return int64(typed)
	default:
		return 0
	}
}
