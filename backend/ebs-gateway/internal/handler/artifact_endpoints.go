package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-gateway/internal/identity"
)

const maxServiceConfigResponse = 5 << 20

type artifactEndpoint struct {
	RunnerInternalURL string `json:"runnerInternalURL" yaml:"runnerInternalURL"`
	RunnerExternalURL string `json:"runnerExternalURL" yaml:"runnerExternalURL"`
}

// ArtifactEndpoints returns the Runner-facing addresses without exposing the OpsOnly Config.
func (a *Handler) ArtifactEndpoints(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if principal(c).Type != identity.RunnerType {
		c.JSON(http.StatusForbidden, gin.H{"error": "Runner identity required"})
		return
	}
	if c.Request.URL.RawQuery != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameters are not supported"})
		return
	}
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet,
		apiPrefix+"/configs/"+ebsv1.ServiceConfigName, nil, nil)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service configuration unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service configuration unavailable"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxServiceConfigResponse+1))
	if err != nil || len(data) > maxServiceConfigResponse {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service configuration unavailable"})
		return
	}
	var config struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Visibility string `json:"visibility"`
			Content    string `json:"content"`
		} `json:"spec"`
	}
	if json.Unmarshal(data, &config) != nil || config.Metadata.Name != ebsv1.ServiceConfigName ||
		config.Spec.Visibility != string(ebsv1.ConfigVisibilityOpsOnly) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invalid service configuration"})
		return
	}
	var content ebsv1.ServiceConfigContent
	decoder := yaml.NewDecoder(strings.NewReader(config.Spec.Content))
	decoder.KnownFields(true)
	var trailing any
	if decoder.Decode(&content) != nil || decoder.Decode(&trailing) != io.EOF {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invalid service configuration"})
		return
	}
	instances := make(map[string]artifactEndpoint, len(content.ArtifactManagers))
	for name, instance := range content.ArtifactManagers {
		instances[name] = artifactEndpoint{
			RunnerInternalURL: instance.RunnerInternalURL,
			RunnerExternalURL: instance.RunnerExternalURL,
		}
	}
	c.JSON(http.StatusOK, gin.H{"instances": instances})
}
