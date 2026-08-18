package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type publicReadContextKey struct{}

var publicProjectResources = map[string]struct{}{
	"snapshots":  {},
	"builds":     {},
	"buildinfos": {},
	"rpmrepos":   {},
	"jobs":       {},
}

func hasAuthorizationHeader(r *http.Request) bool {
	for key := range r.Header {
		if strings.EqualFold(key, "Authorization") {
			return true
		}
	}
	return false
}

func isInternalGlobalAPIPath(path string) bool {
	parts, ok := ebsAPIPathParts(path)
	if !ok || len(parts) == 0 {
		return false
	}
	_, internal := publicProjectResources[parts[0]]
	return internal
}

func isPublicReadRoute(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if hasWatchRequest(r) {
		return false
	}
	parts, ok := ebsAPIPathParts(r.URL.Path)
	if !ok || len(parts) == 0 || parts[0] != "projects" {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	if len(parts) == 2 {
		return validPathSegment(parts[1])
	}
	if len(parts) == 3 && parts[2] == "status" {
		return validPathSegment(parts[1])
	}
	if len(parts) < 3 || !validPathSegment(parts[1]) {
		return false
	}
	if _, ok := publicProjectResources[parts[2]]; !ok {
		return false
	}
	switch len(parts) {
	case 3:
		return true
	case 4:
		return validPathSegment(parts[3])
	case 5:
		return validPathSegment(parts[3]) && parts[4] == "status"
	default:
		return false
	}
}

func (g *Gateway) preparePublicRead(r *http.Request) error {
	if !isPublicCollectionPath(r.URL.Path) {
		return nil
	}
	values, present := r.URL.Query()["limit"]
	if present {
		if len(values) != 1 {
			return fmt.Errorf("invalid limit")
		}
		limit, err := strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > g.cfg.PublicMaxListLimit {
			return fmt.Errorf("limit must be between 1 and %d", g.cfg.PublicMaxListLimit)
		}
		return nil
	}
	query := r.URL.Query()
	query.Set("limit", strconv.Itoa(g.cfg.PublicMaxListLimit))
	r.URL.RawQuery = query.Encode()
	return nil
}

func isPublicCollectionPath(path string) bool {
	parts, ok := ebsAPIPathParts(path)
	if !ok {
		return false
	}
	if len(parts) == 1 && parts[0] == "projects" {
		return true
	}
	if len(parts) != 3 || parts[0] != "projects" || !validPathSegment(parts[1]) {
		return false
	}
	_, ok = publicProjectResources[parts[2]]
	return ok
}

func hasWatchRequest(r *http.Request) bool {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return true
	}
	values, present := query["watch"]
	if !present {
		return false
	}
	for _, value := range values {
		if value != "false" {
			return true
		}
	}
	return false
}

func ebsAPIPathParts(path string) ([]string, bool) {
	if !strings.HasPrefix(path, apiPrefix+"/") {
		return nil, false
	}
	rel := strings.TrimPrefix(path, apiPrefix+"/")
	if rel == "" || strings.HasSuffix(rel, "/") || strings.Contains(rel, "//") {
		return nil, false
	}
	return strings.Split(rel, "/"), true
}

func validPathSegment(value string) bool {
	return value != "" && value != "." && value != ".."
}

func markPublicRead(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), publicReadContextKey{}, true))
}

func isMarkedPublicRead(r *http.Request) bool {
	marked, _ := r.Context().Value(publicReadContextKey{}).(bool)
	return marked
}

func sanitizePublicResponseHeaders(header http.Header) {
	for key := range header {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "x-ebs-") || lower == "etag" || lower == "resource-version" || lower == "x-resource-version" {
			header.Del(key)
		}
	}
}

type headResponseWriter struct {
	http.ResponseWriter
}

func (w headResponseWriter) Write(data []byte) (int, error) {
	return len(data), nil
}
