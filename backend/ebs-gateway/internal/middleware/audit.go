package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"
)

// Audit records request outcomes without logging headers or request bodies.
func Audit(now func() time.Time) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := now()
		method := c.Request.Method
		path := c.Request.URL.Path
		c.Next()
		user := Principal(c)
		subject := user.Subject
		if subject == "" {
			subject = "anonymous"
		}
		klog.InfoS("HTTP request", "method", method, "path", path, "status", c.Writer.Status(),
			"response_bytes", c.Writer.Size(), "latency_ms", now().Sub(started).Milliseconds(),
			"client_ip", c.ClientIP(), "user", subject, "request_id", c.Writer.Header().Get("X-Request-ID"),
			"service_project", c.GetString("serviceProject"), "service_permission", c.GetString("servicePermission"),
			"service_authorization", c.GetString("serviceAuthorization"))
	}
}
