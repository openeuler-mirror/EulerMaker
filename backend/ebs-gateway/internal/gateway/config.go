package gateway

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-gateway/internal/policy"
)

func (a *Handler) configGuard(c *gin.Context) {
	route := policy.Route{Resource: "configs", Name: c.Param("name"), Method: c.Request.Method}
	a.checkResourceAccess(c, route, authenticatedOnly, policy.AuthorizeConfig, noPendingCheck)
}

// readConfigContent reads a fixed, server-selected Config for a Gateway projection.
func (a *Handler) readConfigContent(c *gin.Context, name string, out any) bool {
	const maxResponse = 5 << 20
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, apiPrefix+"/configs/"+name, nil, nil)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	var config ebsv1.Config
	if err != nil || len(data) > maxResponse || json.Unmarshal(data, &config) != nil || config.Name != name {
		return false
	}
	decoder := yaml.NewDecoder(strings.NewReader(config.Spec.Content))
	decoder.KnownFields(true)
	var trailing any
	return decoder.Decode(out) == nil && decoder.Decode(&trailing) == io.EOF
}

func (a *Handler) buildTargets(c *gin.Context) {
	var content ebsv1.BuildTargetContent
	if !a.readConfigContent(c, ebsv1.BuildTargetConfigName, &content) || content.Targets == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "build targets unavailable"})
		return
	}
	type target struct {
		Distro string   `json:"distro"`
		Arches []string `json:"arches"`
	}
	items := make([]target, 0, len(content.Targets))
	for _, distro := range slices.Sorted(maps.Keys(content.Targets)) {
		arches := slices.Sorted(maps.Keys(content.Targets[distro].Arches))
		if len(arches) == 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invalid build targets"})
			return
		}
		items = append(items, target{Distro: distro, Arches: arches})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
