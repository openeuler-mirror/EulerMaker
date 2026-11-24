package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
)

func TestLimitAuthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	clock := time.Unix(1790000000, 0)
	limiter := LimitAuthenticated(limit.New(1, 1, func() time.Time { return clock }))
	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	router.GET("/:user", func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{Subject: c.Param("user")})
	}, limiter, func(c *gin.Context) {
		calls++
		c.Status(http.StatusNoContent)
	})
	for _, tc := range []struct {
		user, address string
		advance       time.Duration
		status        int
	}{
		{"alice", "192.0.2.1:1234", 0, http.StatusNoContent},
		{"alice", "192.0.2.1:1234", 0, http.StatusTooManyRequests},
		{"bob", "192.0.2.1:1234", 0, http.StatusNoContent},
		{"alice", "192.0.2.2:1234", 0, http.StatusNoContent},
		{"alice", "192.0.2.1:1234", time.Second, http.StatusNoContent},
	} {
		clock = clock.Add(tc.advance)
		request := httptest.NewRequest(http.MethodGet, "/"+tc.user, nil)
		request.RemoteAddr = tc.address
		response := httptest.NewRecorder()
		before := calls
		router.ServeHTTP(response, request)
		if response.Code != tc.status || (calls == before+1) != (tc.status == http.StatusNoContent) {
			t.Fatalf("user=%s address=%s status=%d calls=%d", tc.user, tc.address, response.Code, calls-before)
		}
		if tc.status == http.StatusTooManyRequests && response.Header().Get("Retry-After") != "1" {
			t.Fatal("missing retry delay")
		}
	}
}
