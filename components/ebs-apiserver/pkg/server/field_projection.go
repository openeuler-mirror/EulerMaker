package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Field paths are relative to one resource, including for list responses.
// Projection changes only the response; the stored object remains complete.
type fieldProjection struct {
	include *fieldPathNode
	exclude *fieldPathNode
}

type fieldPathNode struct {
	terminal bool
	children map[string]*fieldPathNode
}

type projectionBypassKey struct{}

var projectableResourceKinds = map[string]struct{}{
	"Config": {}, "Script": {}, "Project": {}, "Snapshot": {}, "Build": {},
	"BuildInfo": {}, "RpmRepo": {}, "Job": {}, "Runner": {},
	"User": {}, "MachineAccount": {},
}

func parseFieldProjection(r *http.Request) (fieldProjection, bool, error) {
	query := r.URL.Query()
	include, hasInclude := query["includeFields"]
	exclude, hasExclude := query["excludeFields"]
	if !hasInclude && !hasExclude {
		return fieldProjection{}, false, nil
	}
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/apis/") {
		return fieldProjection{}, true, fmt.Errorf("field projection is supported only for resource GET and LIST")
	}
	if watch := query.Get("watch"); watch != "" && !strings.EqualFold(watch, "false") && watch != "0" {
		return fieldProjection{}, true, fmt.Errorf("field projection is not supported for watch")
	}
	if accept := r.Header.Get("Accept"); accept != "" && accept != "*/*" && !strings.Contains(accept, "application/json") {
		return fieldProjection{}, true, fmt.Errorf("field projection requires a JSON response")
	}
	includeTree, err := parseFieldPaths(include)
	if err != nil {
		return fieldProjection{}, true, fmt.Errorf("invalid includeFields: %w", err)
	}
	excludeTree, err := parseFieldPaths(exclude)
	if err != nil {
		return fieldProjection{}, true, fmt.Errorf("invalid excludeFields: %w", err)
	}
	return fieldProjection{include: includeTree, exclude: excludeTree}, true, nil
}

func parseFieldPaths(values []string) (*fieldPathNode, error) {
	if len(values) == 0 {
		return nil, nil
	}
	root := &fieldPathNode{children: make(map[string]*fieldPathNode)}
	count := 0
	for _, value := range values {
		for _, path := range strings.Split(value, ",") {
			path = strings.TrimSpace(path)
			if path == "" || len(path) > 256 {
				return nil, fmt.Errorf("field paths must be non-empty and at most 256 characters")
			}
			count++
			if count > 64 {
				return nil, fmt.Errorf("at most 64 field paths are allowed")
			}
			node := root
			for _, part := range strings.Split(path, ".") {
				if part == "" || strings.TrimSpace(part) != part {
					return nil, fmt.Errorf("invalid field path %q", path)
				}
				if node.children[part] == nil {
					node.children[part] = &fieldPathNode{children: make(map[string]*fieldPathNode)}
				}
				node = node.children[part]
			}
			node.terminal = true
		}
	}
	return root, nil
}

func withFieldProjection(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(projectionBypassKey{}) != nil {
			next.ServeHTTP(w, r)
			return
		}
		projection, requested, err := parseFieldProjection(r)
		if err != nil {
			writeFieldProjectionError(w, err)
			return
		}
		if !requested {
			next.ServeHTTP(w, r)
			return
		}
		buffer := httptest.NewRecorder()
		next.ServeHTTP(buffer, r.WithContext(context.WithValue(r.Context(), projectionBypassKey{}, true)))
		result := buffer.Result()
		defer result.Body.Close()
		if result.StatusCode < 200 || result.StatusCode >= 300 {
			copyProjectedResponse(w, result.Header, result.StatusCode, buffer.Body.Bytes())
			return
		}
		if !strings.Contains(result.Header.Get("Content-Type"), "application/json") {
			writeFieldProjectionError(w, fmt.Errorf("field projection requires a JSON resource response"))
			return
		}
		body, err := projection.apply(buffer.Body.Bytes())
		if err != nil {
			writeFieldProjectionError(w, err)
			return
		}
		copyProjectedResponse(w, result.Header, result.StatusCode, body)
	})
}

func (p fieldProjection) apply(body []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]interface{}
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("decode resource response: %w", err)
	}
	kind, _ := object["kind"].(string)
	resourceKind := strings.TrimSuffix(kind, "List")
	if _, ok := projectableResourceKinds[resourceKind]; !ok {
		return nil, fmt.Errorf("field projection is supported only for resource GET and LIST")
	}
	if items, isList := object["items"]; isList {
		values, ok := items.([]interface{})
		if !strings.HasSuffix(kind, "List") || (!ok && items != nil) {
			return nil, fmt.Errorf("invalid resource list response")
		}
		if ok {
			for i, item := range values {
				values[i] = p.applyItem(item)
			}
			object["items"] = values
		}
	} else {
		apiVersion := object["apiVersion"]
		projected := p.applyItem(object)
		object, _ = projected.(map[string]interface{})
		if object == nil {
			object = make(map[string]interface{})
		}
		object["kind"] = kind
		if apiVersion != nil {
			object["apiVersion"] = apiVersion
		}
	}
	return json.Marshal(object)
}

func (p fieldProjection) applyItem(value interface{}) interface{} {
	object, _ := value.(map[string]interface{})
	var apiVersion, kind interface{}
	if object != nil {
		apiVersion, kind = object["apiVersion"], object["kind"]
	}
	if p.include != nil {
		value, _ = includeFieldPaths(value, p.include)
	}
	if p.exclude != nil {
		excludeFieldPaths(value, p.exclude)
	}
	if value == nil {
		value = map[string]interface{}{}
	}
	if object, ok := value.(map[string]interface{}); ok {
		if apiVersion != nil {
			object["apiVersion"] = apiVersion
		}
		if kind != nil {
			object["kind"] = kind
		}
	}
	return value
}

func includeFieldPaths(value interface{}, node *fieldPathNode) (interface{}, bool) {
	if node.terminal {
		return value, true
	}
	switch object := value.(type) {
	case map[string]interface{}:
		selected := make(map[string]interface{})
		for key, child := range node.children {
			if field, exists := object[key]; exists {
				if projected, ok := includeFieldPaths(field, child); ok {
					selected[key] = projected
				}
			}
		}
		return selected, len(selected) > 0
	case []interface{}:
		selected := make([]interface{}, len(object))
		for i, item := range object {
			projected, ok := includeFieldPaths(item, node)
			if ok {
				selected[i] = projected
			} else {
				selected[i] = map[string]interface{}{}
			}
		}
		return selected, true
	default:
		return nil, false
	}
}

func excludeFieldPaths(value interface{}, node *fieldPathNode) {
	switch object := value.(type) {
	case map[string]interface{}:
		for key, child := range node.children {
			if child.terminal {
				delete(object, key)
			} else {
				excludeFieldPaths(object[key], child)
			}
		}
	case []interface{}:
		for _, item := range object {
			excludeFieldPaths(item, node)
		}
	}
}

func copyProjectedResponse(w http.ResponseWriter, headers http.Header, status int, body []byte) {
	for key, values := range headers {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeFieldProjectionError(w http.ResponseWriter, err error) {
	status := apierrors.NewBadRequest(err.Error()).ErrStatus
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(status)
}
