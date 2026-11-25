package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
)

type runnerServiceEndpoint struct {
	RunnerInternalURL string `json:"runnerInternalURL" yaml:"runnerInternalURL"`
	RunnerExternalURL string `json:"runnerExternalURL" yaml:"runnerExternalURL"`
}

// serviceEndpoints returns Runner-facing addresses without exposing management URLs or the raw Config.
func (a *Handler) serviceEndpoints(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if middleware.Principal(c).Type != identity.RunnerType {
		c.JSON(http.StatusForbidden, gin.H{"error": "Runner identity required"})
		return
	}
	if c.Request.URL.RawQuery != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameters are not supported"})
		return
	}
	var content ebsv1.ServiceConfigContent
	if !a.readConfigContent(c, ebsv1.ServiceConfigName, &content) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invalid service configuration"})
		return
	}
	instances := make(map[string]runnerServiceEndpoint, len(content.ArtifactManagers))
	for name, instance := range content.ArtifactManagers {
		instances[name] = runnerServiceEndpoint{
			RunnerInternalURL: instance.RunnerInternalURL,
			RunnerExternalURL: instance.RunnerExternalURL,
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"artifactManagers": instances,
		"gitServer": runnerServiceEndpoint{
			RunnerInternalURL: content.GitServer.RunnerInternalURL,
			RunnerExternalURL: content.GitServer.RunnerExternalURL,
		},
	})
}
