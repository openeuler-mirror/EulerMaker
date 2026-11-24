package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/middleware"
)

// serveConfig examines visibility and serves the same upstream response. For
// HEAD it obtains a GET internally so that visibility can still be checked.
func (a *Handler) serveConfig(c *gin.Context) {
	name := c.Param("name")
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, apiPrefix+"/configs/"+url.PathEscape(name), nil, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Config unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.Status(response.StatusCode)
		return
	}
	spool, err := os.CreateTemp("", "ebs-gateway-config-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Config unavailable"})
		return
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	if _, err := io.Copy(spool, response.Body); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Config unavailable"})
		return
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Config unavailable"})
		return
	}
	var object struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Visibility string `json:"visibility"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(spool).Decode(&object); err != nil || object.Metadata.Name != name {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid Config response"})
		return
	}
	if object.Spec.Visibility != "Public" && !middleware.Principal(c).IsPrivileged() {
		c.JSON(http.StatusForbidden, gin.H{"error": "Config access denied"})
		return
	}
	if response.Header.Get("Content-Type") != "" {
		c.Header("Content-Type", response.Header.Get("Content-Type"))
	}
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Config unavailable"})
		return
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, spool)
}
