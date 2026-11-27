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

func TestLimitIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	clock := time.Unix(1790000000, 0)
	now := func() time.Time { return clock }
	limiter := LimitIdentity(limit.New(1, 1, now))
	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	router.GET("/:kind/:user", func(c *gin.Context) {
		if c.Param("kind") != "anonymous" {
			c.Set(principalKey, identity.Principal{Type: identity.Type(c.Param("kind")), Subject: c.Param("user")})
		}
	}, limiter, func(c *gin.Context) {
		calls++
		c.Status(http.StatusNoContent)
	})
	for _, tc := range []struct {
		user, address string
		advance       time.Duration
		status        int
	}{
		{"user/alice", "192.0.2.1:1234", 0, http.StatusNoContent},
		{"user/alice", "192.0.2.1:1234", 0, http.StatusTooManyRequests},
		{"user/bob", "192.0.2.1:1234", 0, http.StatusNoContent},
		{"service/alice", "192.0.2.1:1234", 0, http.StatusNoContent},
		{"runner/alice", "192.0.2.1:1234", 0, http.StatusNoContent},
		{"anonymous/one", "192.0.2.1:1234", 0, http.StatusNoContent},
		{"anonymous/two", "192.0.2.1:1234", 0, http.StatusTooManyRequests},
		{"anonymous/two", "192.0.2.2:1234", 0, http.StatusNoContent},
		{"user/alice", "192.0.2.2:1234", 0, http.StatusNoContent},
		{"user/alice", "192.0.2.1:1234", time.Second, http.StatusNoContent},
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
