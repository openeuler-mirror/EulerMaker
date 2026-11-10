package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"artifact-manager/pkg/diagnostics"
)

type failureKey struct{}

type requestFailure struct {
	code  string
	cause string
}

func recordFailure(r *http.Request, code string, err error) {
	if failure, ok := r.Context().Value(failureKey{}).(*requestFailure); ok {
		if code != "" {
			failure.code = diagnostics.Code(code)
		}
		if err != nil && failure.cause == "" {
			failure.cause = diagnostics.Cause(err)
		}
	}
}

// failureLogger logs only unsuccessful requests; it never inspects headers, queries or bodies.
func failureLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		failure := &requestFailure{}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), failureKey{}, failure))
		defer func() {
			abort := false
			panicked := false
			if recovered := recover(); recovered != nil {
				panicked = true
				// Panic values can contain request data; record only the type.
				failure.code, failure.cause = "Panic", fmt.Sprintf("panic type %T", recovered)
				abort = recovered == http.ErrAbortHandler
				if !abort && !c.Writer.Written() {
					writeErr(c.Writer, c.Request, http.StatusInternalServerError, "InternalError", "internal error", true, nil)
				}
				c.Abort()
			}
			if c.Writer.Status() < 400 && failure.cause == "" {
				return
			}
			code := failure.code
			if code == "" {
				code = "HTTPError"
			}
			// Use the route template, not a raw path that may contain credentials.
			path := c.FullPath()
			if path == "" {
				path = "unmatched"
			}
			resource := func(name string) string {
				value := c.Param(name)
				if validIdentifier(value) {
					return value
				}
				return ""
			}
			fields := []any{
				"component", "artifact-manager", "method", c.Request.Method, "path", path,
				"status", c.Writer.Status(), "code", code, "duration", time.Since(start),
				"project", resource("project"), "job", resource("job"),
				"build", resource("build"), "repository", resource("uid"), "cause", failure.cause,
			}
			if c.Writer.Status() >= 500 || panicked {
				klog.ErrorS(nil, "HTTP request failed", fields...)
			} else {
				klog.InfoS("HTTP request rejected", fields...)
			}
			if abort {
				panic(http.ErrAbortHandler)
			}
		}()
		c.Next()
	}
}
